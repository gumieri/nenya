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

// Span is one recorded stage of a trace — a stage of the client's request
// trace (adopted parent) or of a gateway-rooted trace (linked context). End emits a structured
// slog record (Debug) with duration and status; spans are cheap structs —
// the "exporter" is the process logger, which is what makes this OTel-lite.
type Span struct {
	name      string
	sc        SpanContext
	parentID  SpanID
	start     time.Time
	logger    *slog.Logger
	hasParent bool
	// linkTraceID/linkSpanID carry the CURRENT (innermost) span when the
	// gateway roots its own trace for gateway-initiated work (MCP loop,
	// engine calls): that context is linked for correlation, never adopted as
	// parent (NENYA-187). On the plain request path the current span sits on
	// the client's trace, so the link's trace id correlates with the client's
	// trace. On nested gateway work (an engine call inside the MCP loop) the
	// link points at the enclosing gateway span, whose own link reaches the
	// client context.
	linkTraceID string
	linkSpanID  string
	hasLink     bool
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

// StartGatewaySpan roots a NEW gateway trace for gateway-initiated work (the
// MCP multi-turn loop, engine/judgment calls) and records the current span as
// a LINK rather than a parent (NENYA-187). The policy: a client's traceparent
// is adopted by the request span it actually caused, while work the gateway
// initiates gets gateway-generated ids with the current context linked for
// correlation — a client cannot reshape gateway-owned traces (its ids are
// never adopted as a gateway span's parent), and the propagated context
// upstream is the gateway trace.
func StartGatewaySpan(ctx context.Context, logger *slog.Logger, name string) (context.Context, *Span) {
	if disabled(ctx) {
		return ctx, nil
	}
	traceID, spanID := NewIDs()
	span := &Span{
		name:   name,
		sc:     SpanContext{TraceID: traceID, SpanID: spanID, Sampled: true},
		start:  time.Now(),
		logger: logger,
	}
	if parent, ok := ctx.Value(ctxKey{}).(Span); ok {
		span.linkTraceID = parent.sc.TraceID.String()
		span.linkSpanID = parent.sc.SpanID.String()
		span.hasLink = true
		// Gateway-rooted spans are always recorded locally, but they inherit
		// the linked context's sampling decision so a caller that sampled
		// nothing out does not see gateway spans claim otherwise.
		span.sc.Sampled = parent.sc.Sampled
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
	attrs := []any{
		"span", s.name,
		"trace_id", s.sc.TraceID.String(),
		"span_id", s.sc.SpanID.String(),
		"parent_span_id", s.parentID.String(),
		"has_parent", s.hasParent,
		"duration_ms", float64(time.Since(s.start).Microseconds()) / 1000.0,
		"sampled", s.sc.Sampled,
	}
	if s.hasLink {
		// Gateway-rooted span: the client's context rides as a link.
		attrs = append(attrs, "linked_trace_id", s.linkTraceID, "linked_span_id", s.linkSpanID)
	}
	s.logger.Debug("trace span", attrs...)
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
	usage  *UsageAttributes
}

// UsageAttributes is the GenAI-semantic-convention usage set the request
// summary carries (NENYA-182/187). The values are already normalized by the
// usage layer (provider-specific cache spellings folded into one dimension),
// so consumers see one attribute set regardless of which provider served the
// request.
type UsageAttributes struct {
	// RequestModel is the model this dispatch routed to (gen_ai.request.model
	// — the serving target's model, not necessarily the client's literal
	// alias); ResponseModel is the model the upstream reported serving
	// (gen_ai.response.model), added only when it differs — a silent
	// substitution is visible as a difference between the two.
	RequestModel  string
	ResponseModel string
	Provider      string
	InputTokens   int
	OutputTokens  int
	CachedTokens  int
	CacheCreation int
	Reasoning     int
}

// SetUsage attaches the normalized usage attributes to the request trace.
// Nil-safe. Last call wins by design: usage is committed once per served
// request, and a continuation/failover re-dispatch under the same request
// context replaces the previous attempt's partial totals (billing accounts
// per dispatch, the trace reports the last one served).
func (rt *RequestTrace) SetUsage(u UsageAttributes) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.usage = &u
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

// appendUsageAttributes appends the GenAI-semconv usage attributes for a
// recorded usage set (NENYA-182/187). Detail attributes are omitted when zero
// (and string attributes when empty) so a summary carries no noise.
func appendUsageAttributes(attrs []any, usage *UsageAttributes) []any {
	if usage == nil {
		return attrs
	}
	attrs = append(attrs,
		"gen_ai.usage.input_tokens", usage.InputTokens,
		"gen_ai.usage.output_tokens", usage.OutputTokens,
	)
	if usage.Provider != "" {
		attrs = append(attrs, "gen_ai.system", usage.Provider)
	}
	if usage.RequestModel != "" {
		attrs = append(attrs, "gen_ai.request.model", usage.RequestModel)
	}
	if usage.ResponseModel != "" && usage.ResponseModel != usage.RequestModel {
		attrs = append(attrs, "gen_ai.response.model", usage.ResponseModel)
	}
	if usage.CachedTokens > 0 {
		attrs = append(attrs, "gen_ai.usage.cached_tokens", usage.CachedTokens)
	}
	if usage.CacheCreation > 0 {
		attrs = append(attrs, "gen_ai.usage.cache_creation_input_tokens", usage.CacheCreation)
	}
	if usage.Reasoning > 0 {
		attrs = append(attrs, "gen_ai.usage.reasoning_tokens", usage.Reasoning)
	}
	return attrs
}

// RecordUsage attaches normalized usage attributes to the request trace
// carried by ctx (no-op when untraced or tracing-disabled).
func RecordUsage(ctx context.Context, u UsageAttributes) {
	if disabled(ctx) {
		return
	}
	if rt, ok := ctx.Value(traceKey{}).(*RequestTrace); ok {
		rt.SetUsage(u)
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
	usage := rt.usage
	rt.mu.Unlock()
	attrs := []any{
		"trace", rt.name,
		"trace_id", rt.trace,
		"total_ms", float64(time.Since(rt.start).Microseconds()) / 1000.0,
		"stages", stages,
	}
	attrs = appendUsageAttributes(attrs, usage)
	rt.logger.Info("request trace", attrs...)
}
