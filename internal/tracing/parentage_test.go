package tracing

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// NENYA-187: parentage policy. The request span ADOPTS the client's
// traceparent (client-parented); gateway-initiated work (MCP loop, engine
// calls) roots its OWN trace and LINKS the client's context instead.

func debugLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

const clientTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestRequestSpanAdoptsClientTraceparent(t *testing.T) {
	var buf bytes.Buffer
	ctx, rt := StartRequest(context.Background(), debugLogger(&buf), clientTraceparent, "chat_completions")
	defer rt.Finish()

	// The request span continues the client's trace id.
	if got := CurrentTraceID(ctx); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace id = %q, want the client's trace id (adopted)", got)
	}
	if !ContinuedInbound(ctx) {
		t.Fatal("the request span must continue the inbound traceparent")
	}
	// Propagation to the upstream stays on the client's trace.
	if tp := OutgoingTraceparent(ctx); !strings.HasPrefix(tp, "00-4bf92f3577b34da6a3ce929d0e0e4736-") {
		t.Fatalf("outgoing traceparent = %q, want the client trace", tp)
	}
}

func TestGatewaySpanRootsOwnTraceAndLinksClient(t *testing.T) {
	var buf bytes.Buffer
	ctx, rt := StartRequest(context.Background(), debugLogger(&buf), clientTraceparent, "chat_completions")
	defer rt.Finish()

	clientTrace := CurrentTraceID(ctx)
	loopCtx, span := StartGatewaySpan(ctx, debugLogger(&buf), "upstream.mcp_loop")

	gatewayTrace := CurrentTraceID(loopCtx)
	if gatewayTrace == clientTrace {
		t.Fatal("gateway-initiated work must root its OWN trace, not adopt the client's")
	}
	if ContinuedInbound(loopCtx) {
		t.Fatal("a gateway-rooted span has no adopted parent (tracestate must be discarded)")
	}
	if !span.hasLink || span.linkTraceID != clientTrace {
		t.Fatalf("the client context must be LINKED: hasLink=%v linked=%q want %q", span.hasLink, span.linkTraceID, clientTrace)
	}
	// The client's span id is linked, and it is NOT the parent.
	if span.linkSpanID == "" {
		t.Fatal("the client's span id must be linked")
	}
	if span.parentID != (SpanID{}) || span.hasParent {
		t.Fatalf("a gateway-rooted span must have no adopted parent: parent=%v hasParent=%v", span.parentID, span.hasParent)
	}
	// Work dispatched under the gateway span propagates the gateway trace.
	if tp := OutgoingTraceparent(loopCtx); !strings.HasPrefix(tp, "00-"+gatewayTrace+"-") {
		t.Fatalf("outgoing traceparent = %q, want the gateway trace %s", tp, gatewayTrace)
	}
	span.End()
	if !strings.Contains(buf.String(), "linked_trace_id") {
		t.Fatalf("the span record must carry the link:\n%s", buf.String())
	}
}

func TestGatewaySpanWithoutInboundTrace(t *testing.T) {
	ctx, span := StartGatewaySpan(context.Background(), debugLogger(&bytes.Buffer{}), "engine.call")
	defer span.End()
	if span.hasLink {
		t.Fatal("no client context to link when the request was untraced")
	}
	if CurrentTraceID(ctx) == "" {
		t.Fatal("a gateway span still roots a trace")
	}
}

func TestGatewaySpanDisabledIsInert(t *testing.T) {
	ctx, span := StartGatewaySpan(Disable(context.Background()), debugLogger(&bytes.Buffer{}), "engine.call")
	span.End() // nil span, must not panic
	if CurrentTraceID(ctx) != "" {
		t.Fatal("tracing-disabled context must stay untraced")
	}
}

func TestRequestTraceUsageAttributes(t *testing.T) {
	var buf bytes.Buffer
	ctx, rt := StartRequest(context.Background(), debugLogger(&buf), "", "chat_completions")

	RecordUsage(ctx, UsageAttributes{
		RequestModel: "claude-sonnet-4", ResponseModel: "claude-sonnet-4-20250514", Provider: "anthropic",
		InputTokens: 1200, OutputTokens: 340, CachedTokens: 900, CacheCreation: 120, Reasoning: 55,
	})
	rt.Finish()

	out := buf.String()
	for _, want := range []string{
		"gen_ai.system=anthropic",
		"gen_ai.request.model=claude-sonnet-4",
		"gen_ai.response.model=claude-sonnet-4-20250514",
		"gen_ai.usage.input_tokens=1200",
		"gen_ai.usage.output_tokens=340",
		"gen_ai.usage.cached_tokens=900",
		"gen_ai.usage.cache_creation_input_tokens=120",
		"gen_ai.usage.reasoning_tokens=55",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("trace summary missing %q:\n%s", want, out)
		}
	}
}

func TestRequestTraceUsageAttributesOmitZeroDetails(t *testing.T) {
	var buf bytes.Buffer
	ctx, rt := StartRequest(context.Background(), debugLogger(&buf), "", "chat_completions")
	RecordUsage(ctx, UsageAttributes{RequestModel: "m", Provider: "p", InputTokens: 10, OutputTokens: 2})
	rt.Finish()

	out := buf.String()
	if strings.Contains(out, "cached_tokens") || strings.Contains(out, "cache_creation") || strings.Contains(out, "reasoning_tokens") {
		t.Fatalf("zero-valued detail attributes must be omitted:\n%s", out)
	}
	if !strings.Contains(out, "gen_ai.usage.input_tokens=10") {
		t.Fatalf("base attributes must be present:\n%s", out)
	}
}
