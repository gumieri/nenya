package proxy

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
	"github.com/nenya/internal/tracing"
)

// NENYA-187: the streaming usage callback must feed the request trace the
// CUMULATIVE normalized attributes (not the last chunk's delta) — the round-1
// defect lived exactly here, so the wiring is pinned end-to-end.
func TestUsageCallbackRecordsCumulativeTraceAttributes(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	gw := &gateway.NenyaGateway{
		Logger:      logger,
		Stats:       infra.NewUsageTracker(),
		Calibration: infra.NewCalibrationTracker(infra.CalibrationParams{Enabled: true, MinObservations: 10, Decay: 0.99, ClampMin: 0.5, ClampMax: 4.0}),
	}
	target := routing.UpstreamTarget{Model: "gpt-test", Provider: "prov", CoolKey: "agent:prov:gpt-test"}

	ctx, rt := tracing.StartRequest(context.Background(), logger, "", "chat_completions")
	p := &Proxy{}
	cb := p.makeUsageCallback(ctx, gw, target, "agent")

	// Chunk 1 carries the reasoning delta; chunk 2 carries none (the exact
	// ordering that used to erase the attribute).
	cb(stream.UsageData{Model: "gpt-test-served", PromptTokens: 200, CompletionTokens: 50, ReasoningTokens: 40})
	cb(stream.UsageData{Model: "gpt-test-served", PromptTokens: 0, CompletionTokens: 10, ReasoningTokens: 0})
	rt.Finish()

	out := buf.String()
	for _, want := range []string{
		"gen_ai.request.model=gpt-test",
		"gen_ai.response.model=gpt-test-served",
		"gen_ai.usage.input_tokens=200",
		"gen_ai.usage.output_tokens=60",
		"gen_ai.usage.reasoning_tokens=40",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("request-trace summary missing %q:\n%s", want, out)
		}
	}
}

// NENYA-187: the engine-call path roots a gateway trace — the client's trace
// is linked, never adopted (a regression to StartSpan would adopt it).
func TestEngineCallRootsGatewayTrace(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, rt := tracing.StartRequest(context.Background(), logger, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "chat_completions")
	defer rt.Finish()
	clientTrace := tracing.CurrentTraceID(ctx)

	// The engine span is the gateway-rooted primitive the chain uses: assert
	// the trace id differs from the client's and the link carries it.
	engineCtx, span := tracing.StartGatewaySpan(ctx, logger, "engine.call")
	span.End()
	if got := tracing.CurrentTraceID(engineCtx); got == clientTrace || got == "" {
		t.Fatalf("engine span trace = %q, want a gateway-rooted trace distinct from %q", got, clientTrace)
	}
	if !strings.Contains(buf.String(), "linked_trace_id="+clientTrace) {
		t.Fatalf("engine span must link the client trace:\n%s", buf.String())
	}
}
