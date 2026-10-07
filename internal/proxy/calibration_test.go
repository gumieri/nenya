package proxy

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
)

// TestUsageCallbackCalibratesActual verifies the NENYA-135 actual-side
// wiring: the streaming usage callback feeds cumulative prompt tokens
// (as deltas) into the calibration tracker. This is the proving test for
// the wiring — the tracker is nil-safe, so a silently dropped call would
// otherwise be invisible.
func TestUsageCallbackCalibratesActual(t *testing.T) {
	gw := &gateway.NenyaGateway{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Stats:       infra.NewUsageTracker(),
		Calibration: infra.NewCalibrationTracker(infra.CalibrationParams{Enabled: true, MinObservations: 10, Decay: 0.99, ClampMin: 0.5, ClampMax: 4.0}),
	}
	target := routing.UpstreamTarget{Model: "calibrated-model", Provider: "prov", CoolKey: "agent:prov:calibrated-model"}

	p := &Proxy{}
	cb := p.makeUsageCallback(context.Background(), gw, target, "agent")

	// UsageData.PromptTokens is the per-event DELTA (the stream reader
	// clamps against its own last-value tracker): deltas 100 + 150.
	cb(stream.UsageData{PromptTokens: 100, CompletionTokens: 1})
	cb(stream.UsageData{PromptTokens: 150, CompletionTokens: 2})

	// EMA math: first delta decays before the second is added
	// (100*0.99 + 150 = 249) — equality pins the exact expected sum.
	snap := gw.Calibration.Snapshot()["calibrated-model"]
	if snap.ActualTokens != 249 {
		t.Fatalf("actual tokens = %v, want 249 (decayed EMA of cumulative deltas)", snap.ActualTokens)
	}
	// Estimates recorded at dispatch converge the ratio toward 1.0 for a
	// perfect 1:1 pairing (decayed sums are not exactly equal after three
	// records, so assert convergence, not identity).
	gw.Calibration.RecordEstimate("calibrated-model", 249)
	if got := gw.Calibration.Ratio("calibrated-model"); got < 0.95 || got > 1.05 {
		t.Fatalf("perfect 1:1 pairing must calibrate to ~1.0, got %v", got)
	}
}

// TestRecordMCPUsageStats pins the Stats side of the MCP buffered path:
// recordMCPUsage extracts the nested usage map from the full SSE chunk
// (regression guard — passing the whole chunk silently zeroed output
// tokens) and records output tokens for the response-reported model.
func TestRecordMCPUsageStats(t *testing.T) {
	gw := &gateway.NenyaGateway{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Stats:  infra.NewUsageTracker(),
	}
	buf := &bufferedSSE{rawBytes: []byte(strings.Join([]string{
		`data: {"model":"response-model","choices":[{"delta":{}}],"usage":{"prompt_tokens":50,"completion_tokens":7,"total_tokens":57}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"))}

	(&Proxy{}).recordMCPUsage(gw, buf, "agent")

	snap := gw.Stats.Snapshot()
	models, _ := snap["models"].(map[string]interface{})
	m, ok := models["response-model"].(map[string]interface{})
	if !ok {
		t.Fatalf("response model missing from stats: %v", snap)
	}
	if out, _ := m["output_tokens"].(uint64); out == 0 {
		t.Fatalf("output tokens not recorded: %v", m)
	}
}
