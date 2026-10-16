package proxy

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
)

// substitutionGW builds a gateway whose catalog prices both the requested
// and the served model (distinct rates) and captures the log output.
func substitutionGW(t *testing.T, logBuf *bytes.Buffer) *gateway.NenyaGateway {
	t.Helper()
	catalog := discovery.NewModelCatalog()
	catalog.Add(discovery.DiscoveredModel{ID: "requested-model", Provider: "agg", Pricing: &discovery.PricingEntry{
		InputCostPer1M: 1.00, OutputCostPer1M: 2.00,
	}})
	catalog.Add(discovery.DiscoveredModel{ID: "served-model", Provider: "agg", Pricing: &discovery.PricingEntry{
		InputCostPer1M: 10.00, OutputCostPer1M: 20.00,
	}})
	return &gateway.NenyaGateway{
		Providers:    map[string]*config.Provider{"agg": {Name: "agg"}},
		ModelCatalog: catalog,
		CostTracker:  infra.NewCostTracker(),
		Metrics:      infra.NewMetrics(),
		Logger:       slog.New(slog.NewTextHandler(logBuf, nil)),
	}
}

// TestModelSubstitutionBufferedAttributesServed pins NENYA-173: a buffered
// response reporting a different model than requested is warned, counted,
// and its cost is attributed to the SERVED model's rate card (honest
// billing).
func TestModelSubstitutionBufferedAttributesServed(t *testing.T) {
	var logBuf bytes.Buffer
	gw := substitutionGW(t, &logBuf)
	target := routing.UpstreamTarget{Provider: "agg", Model: "requested-model"}

	recordCostAndBilling(context.Background(), gw, target, "served-model", 1_000_000, 0, 0)

	if got := gw.CostTracker.GetCostMicroUSD("served-model"); got != 10_000_000 {
		t.Fatalf("cost must be attributed to the served model: %d microUSD", got)
	}
	if got := gw.CostTracker.GetCostMicroUSD("requested-model"); got != 0 {
		t.Fatalf("the requested model must not be charged: %d microUSD", got)
	}
	if !strings.Contains(logBuf.String(), "silent substitution") {
		t.Fatalf("expected the substitution warning, got:\n%s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "served-model") || !strings.Contains(logBuf.String(), "requested-model") {
		t.Fatalf("the warning must name both models, got:\n%s", logBuf.String())
	}
	wantSeries(t, gw.Metrics,
		`nenya_model_substitutions_total{requested="requested-model", served="served-model"} 1`)
}

// TestModelSubstitutionUnpricedServedFallsBack pins the fallback: a served
// model without a rate card prices the requested model (flagged in the
// warning) instead of silently dropping the cost.
func TestModelSubstitutionUnpricedServedFallsBack(t *testing.T) {
	var logBuf bytes.Buffer
	gw := substitutionGW(t, &logBuf)
	target := routing.UpstreamTarget{Provider: "agg", Model: "requested-model"}

	recordCostAndBilling(context.Background(), gw, target, "unknown-model", 1_000_000, 0, 0)

	if got := gw.CostTracker.GetCostMicroUSD("requested-model"); got != 1_000_000 {
		t.Fatalf("unpriced served model must fall back to the requested rate: %d microUSD", got)
	}
	if !strings.Contains(logBuf.String(), "silent substitution") {
		t.Fatal("the substitution must still be visible")
	}
}

// TestNoSubstitutionNoWarning pins the quiet path: a matching (or absent)
// response model neither warns nor counts.
func TestNoSubstitutionNoWarning(t *testing.T) {
	var logBuf bytes.Buffer
	gw := substitutionGW(t, &logBuf)
	target := routing.UpstreamTarget{Provider: "agg", Model: "requested-model"}

	recordCostAndBilling(context.Background(), gw, target, "requested-model", 1_000_000, 0, 0)
	recordCostAndBilling(context.Background(), gw, target, "", 1_000_000, 0, 0)

	if strings.Contains(logBuf.String(), "silent substitution") {
		t.Fatalf("no substitution must warn: %s", logBuf.String())
	}
	if got := gw.CostTracker.GetCostMicroUSD("requested-model"); got != 2_000_000 {
		t.Fatalf("requested-model cost = %d, want 2000000", got)
	}
}

// TestModelSubstitutionStreaming pins the streaming path: the usage
// callback carries the served model, reports the substitution once, and
// prices the stream under the served model's rate card.
func TestModelSubstitutionStreaming(t *testing.T) {
	var logBuf bytes.Buffer
	gw := substitutionGW(t, &logBuf)
	gw.Calibration = infra.NewCalibrationTracker(infra.CalibrationParams{Enabled: true, Decay: 0.1, ClampMin: 0.5, ClampMax: 2.0})
	gw.Stats = infra.NewUsageTracker()
	target := routing.UpstreamTarget{Provider: "agg", Model: "requested-model"}

	cb := (&Proxy{}).makeUsageCallback(context.Background(), gw, target, "agent")
	cb(stream.UsageData{PromptTokens: 1_000_000, TotalTokens: 1_000_000, Model: "served-model"})
	// A second chunk must not double-report the substitution.
	cb(stream.UsageData{PromptTokens: 0, CompletionTokens: 1_000_000, TotalTokens: 1_000_000, Model: "served-model"})

	if got := gw.CostTracker.GetCostMicroUSD("served-model"); got != 30_000_000 {
		t.Fatalf("streaming cost must use the served rate card: %d microUSD (want 30000000 = 10+20 per 1M)", got)
	}
	if got := gw.CostTracker.GetCostMicroUSD("requested-model"); got != 0 {
		t.Fatalf("the requested model must not be charged: %d", got)
	}
	if n := strings.Count(logBuf.String(), "silent substitution"); n != 1 {
		t.Fatalf("the substitution must be reported exactly once, got %d:\n%s", n, logBuf.String())
	}
	wantSeries(t, gw.Metrics,
		`nenya_model_substitutions_total{requested="requested-model", served="served-model"} 1`)
}

// wantSeries asserts the rendered Prometheus exposition contains the
// exact series line (label keys AND value), so a missing counter cannot
// pass on a stray HELP line.
func wantSeries(t *testing.T, m *infra.Metrics, want string) {
	t.Helper()
	var buf bytes.Buffer
	m.WritePrometheus(&buf)
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("expected the exact series %q in the exposition, got: %s", want, buf.String())
	}
}

// TestResponseModelOfAnthropicNested pins the nested model surface
// (NENYA-173 review): an Anthropic-native message_start nests the model
// under "message", so a top-level read would never detect substitution on
// Anthropic upstreams.
func TestResponseModelOfAnthropicNested(t *testing.T) {
	msg := map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"model": "claude-nested-model",
			"usage": map[string]interface{}{"input_tokens": float64(10)},
		},
	}
	if got := stream.ResponseModelOf(msg); got != "claude-nested-model" {
		t.Fatalf("nested Anthropic model not read: %q", got)
	}
	if got := stream.ResponseModelOf(map[string]interface{}{"model": "flat-model"}); got != "flat-model" {
		t.Fatalf("flat model not read: %q", got)
	}
	if got := stream.ResponseModelOf(map[string]interface{}{"modelVersion": "gemini-2.5-flash-001"}); got != "gemini-2.5-flash-001" {
		t.Fatalf("modelVersion not read: %q", got)
	}
	if got := stream.ResponseModelOf(map[string]interface{}{"delta": map[string]interface{}{}}); got != "" {
		t.Fatalf("model-less chunk must yield empty, got %q", got)
	}
}

// TestModelSubstitutionStreamingLateModelReattributes pins the review's
// high finding: a served model that only appears on a LATER usage chunk
// must re-resolve the attribution (the cached pricing from the first
// chunk would otherwise charge the requested model forever).
func TestModelSubstitutionStreamingLateModelReattributes(t *testing.T) {
	var logBuf bytes.Buffer
	gw := substitutionGW(t, &logBuf)
	gw.Calibration = infra.NewCalibrationTracker(infra.CalibrationParams{Enabled: true, Decay: 0.1, ClampMin: 0.5, ClampMax: 2.0})
	gw.Stats = infra.NewUsageTracker()
	target := routing.UpstreamTarget{Provider: "agg", Model: "requested-model"}

	cb := (&Proxy{}).makeUsageCallback(context.Background(), gw, target, "agent")
	// First usage chunk: no model reported yet.
	cb(stream.UsageData{PromptTokens: 1_000_000, TotalTokens: 1_000_000})
	if got := gw.CostTracker.GetCostMicroUSD("requested-model"); got != 1_000_000 {
		t.Fatalf("unattributed first chunk must price the requested model: %d", got)
	}
	// Later chunk reveals the served model: the remaining output must be
	// priced under the served card, and the substitution reported.
	cb(stream.UsageData{CompletionTokens: 1_000_000, TotalTokens: 1_000_000, Model: "served-model"})
	// The served model's ledger starts at zero and books its whole
	// cumulative ($10 input + $20 output = $30); the 1M already charged to
	// the requested card stays there. Never a negative delta (the
	// flip-back case below).
	if got := gw.CostTracker.GetCostMicroUSD("served-model"); got != 30_000_000 {
		t.Fatalf("late-discovered served model did not re-attribute: %d microUSD (want 30000000)", got)
	}

	// Flip-back: a later chunk echoing the (cheaper) requested id must not
	// book a negative delta against the requested card, and must not
	// double-charge: the requested ledger holds exactly its earlier 1M.
	cb(stream.UsageData{TotalTokens: 3_000_000, PromptTokens: 1_000_000, CompletionTokens: 2_000_000, Model: "requested-model"})
	if got := gw.CostTracker.GetCostMicroUSD("requested-model"); got < 1_000_000 {
		t.Fatalf("a model flip-back must never credit budget back: %d microUSD", got)
	}
	if got := gw.CostTracker.GetCostMicroUSD("served-model"); got != 30_000_000 {
		t.Fatalf("the flip-back must not alter the served ledger: %d", got)
	}
	if n := strings.Count(logBuf.String(), "silent substitution"); n != 1 {
		t.Fatalf("expected exactly one substitution warning, got %d", n)
	}
	// The counter label is catalog-bounded: an unknown served model folds
	// into "unknown" instead of creating unbounded series.
	cb2 := (&Proxy{}).makeUsageCallback(context.Background(), gw, target, "agent")
	cb2(stream.UsageData{PromptTokens: 1, TotalTokens: 1, Model: "free-form-upstream-string"})
	wantSeries(t, gw.Metrics,
		`nenya_model_substitutions_total{requested="requested-model", served="unknown"} 1`)
}
