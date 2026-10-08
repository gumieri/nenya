package tracing

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

// validTP is a well-formed sampled traceparent.
const validTP = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{}, nil))
}

type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestParseTraceparent(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantOK  bool
		traceID string
		sampled bool
	}{
		{name: "valid sampled", raw: validTP, wantOK: true, traceID: "4bf92f3577b34da6a3ce929d0e0e4736", sampled: true},
		{name: "valid unsampled", raw: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00", wantOK: true, traceID: "4bf92f3577b34da6a3ce929d0e0e4736", sampled: false},
		{name: "wrong length", raw: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7", wantOK: false},
		{name: "forbidden version ff", raw: "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", wantOK: false},
		{name: "non-hex version", raw: "zz-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", wantOK: false},
		{name: "all-zero trace id", raw: "00-00000000000000000000000000000000-00f067aa0ba902b7-01", wantOK: false},
		{name: "all-zero span id", raw: "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01", wantOK: false},
		{name: "non-hex trace id", raw: "00-4bf92f3577b34da6a3ce929d0e0e473g-00f067aa0ba902b7-01", wantOK: false},
		{name: "missing dashes", raw: validTP[:20] + validTP[21:], wantOK: false},
		{name: "version 01 with extension ignored (forward compat)", raw: "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra-extension-bytes", wantOK: true, traceID: "4bf92f3577b34da6a3ce929d0e0e4736", sampled: true},
		{name: "version 00 with trailing garbage rejected", raw: validTP + "-extra", wantOK: false},
		{name: "empty", raw: "", wantOK: false},
		{name: "whitespace trimmed", raw: "  " + validTP + "  ", wantOK: true, traceID: "4bf92f3577b34da6a3ce929d0e0e4736", sampled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, ok := ParseTraceparent(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("ParseTraceparent(%q) ok = %v, want %v", tc.raw, ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if sc.TraceID.String() != tc.traceID {
				t.Errorf("trace id = %s, want %s", sc.TraceID, tc.traceID)
			}
			if sc.Sampled != tc.sampled {
				t.Errorf("sampled = %v, want %v", sc.Sampled, tc.sampled)
			}
		})
	}
}

func TestFormatRoundtrip(t *testing.T) {
	sc, ok := ParseTraceparent(validTP)
	if !ok {
		t.Fatal("fixture must parse")
	}
	if got := FormatTraceparent(sc); got != validTP {
		t.Fatalf("FormatTraceparent = %q, want the canonical input", got)
	}
	// Unsampling flips only the flags field.
	sc.Sampled = false
	if got := FormatTraceparent(sc); !strings.HasSuffix(got, "-00") {
		t.Fatalf("unsampled flags = %q, want -00 suffix", got)
	}
}

func TestNewIDsNotZero(t *testing.T) {
	id, span := NewIDs()
	if id == (TraceID{}) || span == (SpanID{}) {
		t.Fatal("generated ids must not be zero")
	}
}

// TestTraceLifecycle verifies the context derivation contract: a request
// trace continues an inbound traceparent, child spans share the trace id,
// stages land in the summary, and the outgoing header carries the current
// span as parent.
func TestTraceLifecycle(t *testing.T) {
	ctx, rt := StartRequest(context.Background(), testLogger(), validTP, "chat_completions")

	// Inbound trace continued.
	if got := CurrentTraceID(ctx); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace id = %s, want the inbound one", got)
	}

	childCtx, span := StartSpan(ctx, testLogger(), "pipeline.interceptors")
	if got := CurrentTraceID(childCtx); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("child span changed the trace id: %s", got)
	}
	if span.sc.SpanID == (SpanID{}) {
		t.Error("child span id must be generated")
	}
	span.End()
	RecordStage(childCtx, "pipeline.interceptors")

	// Outgoing propagation: same trace, child span as parent.
	tp := OutgoingTraceparent(childCtx)
	parsed, ok := ParseTraceparent(tp)
	if !ok {
		t.Fatalf("outgoing traceparent %q does not parse", tp)
	}
	if parsed.TraceID != span.sc.TraceID || parsed.SpanID != span.sc.SpanID {
		t.Fatalf("outgoing = %s, want the child span context", tp)
	}

	rt.Finish() // smoke: must not panic
}

// TestStartTraceWithoutInbound roots a fresh trace.
func TestStartTraceWithoutInbound(t *testing.T) {
	ctx, _ := StartRequest(context.Background(), testLogger(), "", "chat_completions")
	id := CurrentTraceID(ctx)
	if len(id) != 32 {
		t.Fatalf("fresh trace id = %q, want 32 hex chars", id)
	}
}

// TestDisabledContext verifies the governance-disabled contract: spans,
// stages, and summaries are fully inert (no rand reads, no records).
func TestDisabledContext(t *testing.T) {
	dctx := Disable(context.Background())
	ctx, span := StartSpan(dctx, testLogger(), "stage")
	if ctx != dctx || span != nil {
		t.Fatalf("disabled StartSpan must return the same ctx and a nil span, got ctx-moved=%v span=%v", ctx != dctx, span)
	}
	span.End() // nil-safe no-op
	RecordStage(dctx, "stage")
	if got := CurrentTraceID(dctx); got != "" {
		t.Fatalf("disabled trace id = %q, want empty", got)
	}
	if got := OutgoingTraceparent(dctx); got != "" {
		t.Fatalf("disabled outgoing = %q, want empty", got)
	}
	if ContinuedInbound(dctx) {
		t.Fatal("disabled ContinuedInbound = true, want false")
	}
	// StartRequest on a disabled context also stays inert.
	ctx2, rt := StartRequest(Disable(context.Background()), testLogger(), validTP, "chat")
	if got := CurrentTraceID(ctx2); got != "" {
		t.Fatalf("disabled StartRequest traced id = %q, want empty", got)
	}
	rt.Finish() // no-op, must not panic
}

// TestUntracedContext verifies the no-op paths.
func TestUntracedContext(t *testing.T) {
	ctx := context.Background()
	if got := CurrentTraceID(ctx); got != "" {
		t.Fatalf("untraced trace id = %q, want empty", got)
	}
	if got := OutgoingTraceparent(ctx); got != "" {
		t.Fatalf("untraced outgoing = %q, want empty", got)
	}
	RecordStage(ctx, "nothing") // must not panic
}
