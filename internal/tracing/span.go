package tracing

import (
	"context"
	"crypto/rand"
	"log/slog"
	"sync"
	"time"
)

type ctxKey struct{}

// disabledKey marks a context as tracing-disabled: StartSpan/RecordStage
// become no-ops and the request summary is skipped (governance.tracing
// disabled, NENYA-140).
type disabledKey struct{}

// Disable marks a context tracing-disabled. Call sites that pass the
// marked context through see inert tracing (no spans, no summary, no
// propagation) without branching at each stage.
func Disable(ctx context.Context) context.Context {
	return context.WithValue(ctx, disabledKey{}, true)
}

func disabled(ctx context.Context) bool {
	v, ok := ctx.Value(disabledKey{}).(bool)
	return ok && v
}

// Span is one recorded stage of a request trace. End emits a structured
// slog record (Debug) with duration and status; spans are cheap structs —
// the "exporter" is the process logger, which is what makes this OTel-lite.
type Span struct {
	name      string
	sc        SpanContext
	parentID  SpanID
	start     time.Time
	logger    *slog.Logger
	hasParent bool
}

// StartSpan returns a child span on the ctx's trace (or a fresh trace when
// none exists) and the derived context. The span MUST be ended; the
// deferred-End pattern is expected at call sites.
func StartSpan(ctx context.Context, logger *slog.Logger, name string) (context.Context, *Span) {
	if disabled(ctx) {
		return ctx, nil
	}
	if parent, ok := ctx.Value(ctxKey{}).(Span); ok {
		child := &Span{
			name:      name,
			sc:        SpanContext{TraceID: parent.sc.TraceID, SpanID: NewSpanID(), Sampled: parent.sc.Sampled},
			parentID:  parent.sc.SpanID,
			start:     time.Now(),
			logger:    logger,
			hasParent: true,
		}
		return context.WithValue(ctx, ctxKey{}, *child), child
	}
	return StartTrace(ctx, logger, name, "")
}

// NewSpanID generates a fresh span identifier.
func NewSpanID() SpanID {
	var span SpanID
	_, _ = rand.Read(span[:])
	span[0] |= 0x01
	return span
}

// StartTrace roots a new trace (fresh ids, ignore any inbound parent) or —
// when inboundParent is a valid W3C traceparent — continues that trace with
// a fresh root span. Returns the traced context and the root span.
func StartTrace(ctx context.Context, logger *slog.Logger, name, inboundParent string) (context.Context, *Span) {
	if disabled(ctx) {
		return ctx, nil
	}
	traceID, spanID := NewIDs()
	sc := SpanContext{TraceID: traceID, SpanID: spanID, Sampled: true}
	span := &Span{name: name, sc: sc, start: time.Now(), logger: logger}
	if inbound, ok := ParseTraceparent(inboundParent); ok {
		span.sc.TraceID = inbound.TraceID
		span.sc.Sampled = inbound.Sampled
		span.hasParent = true
		span.parentID = inbound.SpanID
	}
	return context.WithValue(ctx, ctxKey{}, *span), span
}

// End completes the span, emitting one structured slog record. Safe to call
// on a nil span.
func (s *Span) End() {
	if s == nil || s.logger == nil || !s.logger.Enabled(context.Background(), slog.LevelDebug) {
		// Skip the hex encodings when Debug is off (the common case).
		return
	}
	s.logger.Debug(
		"trace span",
		"span", s.name,
		"trace_id", s.sc.TraceID.String(),
		"span_id", s.sc.SpanID.String(),
		"parent_span_id", s.parentID.String(),
		"has_parent", s.hasParent,
		"duration_ms", float64(time.Since(s.start).Microseconds())/1000.0,
		"sampled", s.sc.Sampled,
	)
}

// CurrentTraceID returns the current trace's id, or "" when untraced or
// tracing-disabled.
func CurrentTraceID(ctx context.Context) string {
	if disabled(ctx) {
		return ""
	}
	if span, ok := ctx.Value(ctxKey{}).(Span); ok {
		return span.sc.TraceID.String()
	}
	return ""
}

// ContinuedInbound reports whether the active span continues an inbound
// traceparent (false = the gateway rooted a fresh trace, so inbound
// tracestate must be discarded per W3C §4.2). False when untraced.
func ContinuedInbound(ctx context.Context) bool {
	if disabled(ctx) {
		return false
	}
	if span, ok := ctx.Value(ctxKey{}).(Span); ok {
		return span.hasParent
	}
	return false
}

// OutgoingTraceparent renders the W3C traceparent for propagating the
// CURRENT span to an upstream (same trace, this span as parent). Empty when
// untraced.
func OutgoingTraceparent(ctx context.Context) string {
	if disabled(ctx) {
		return ""
	}
	if span, ok := ctx.Value(ctxKey{}).(Span); ok {
		return FormatTraceparent(span.sc)
	}
	return ""
}

// RequestTrace accumulates the stage names for the request summary emitted
// by Finish (the OTel-lite substitute for a collector payload).
type RequestTrace struct {
	logger *slog.Logger
	name   string
	trace  string
	start  time.Time
	mu     sync.Mutex
	stages []string
}

type traceKey struct{}

// StartRequest roots (or continues) the request trace and returns the
// traced context plus a finish function — call via defer at the handler
// boundary; Finish emits the Info-level summary.
func StartRequest(ctx context.Context, logger *slog.Logger, inboundParent, name string) (context.Context, *RequestTrace) {
	if disabled(ctx) {
		// Disabled: inert finish (Finish skips empty traces), untraced ctx.
		return ctx, &RequestTrace{logger: logger}
	}
	tctx, root := StartTrace(ctx, logger, name, inboundParent)
	rt := &RequestTrace{logger: logger, name: name, trace: root.sc.TraceID.String(), start: root.start}
	tctx = context.WithValue(tctx, traceKey{}, rt)
	return tctx, rt
}

// RecordStage appends a stage name to the request summary. Stages are
// recorded at entry (the summary names the stages a request traversed,
// including ones that aborted mid-stage). No-op when the context carries
// no request trace or tracing is disabled.
func RecordStage(ctx context.Context, name string) {
	if disabled(ctx) {
		return
	}
	if rt, ok := ctx.Value(traceKey{}).(*RequestTrace); ok {
		rt.Stage(name)
	}
}

// Stage records a stage name (called at stage entry; the summary names
// the stages a request traversed).
func (rt *RequestTrace) Stage(name string) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.stages = append(rt.stages, name)
}

// Finish emits the request summary. Nil-safe.
func (rt *RequestTrace) Finish() {
	if rt == nil || rt.logger == nil || rt.trace == "" {
		// Empty trace = disabled request: nothing to summarize.
		return
	}
	rt.mu.Lock()
	stages := append([]string(nil), rt.stages...)
	rt.mu.Unlock()
	rt.logger.Info("request trace",
		"trace", rt.name,
		"trace_id", rt.trace,
		"total_ms", float64(time.Since(rt.start).Microseconds())/1000.0,
		"stages", stages,
	)
}
