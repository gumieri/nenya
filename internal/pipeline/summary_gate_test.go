package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
)

// newFidelityHarness extends the summarize harness with a fidelity
// engine and a wired gate. fidelityVerdict shapes the gate's JSON
// response; fidelityCalls counts gate consultations.
func newFidelityHarness(t *testing.T, fidelityBody string, status int, strict bool) (WindowDeps, config.WindowConfig, *atomicCounter, *atomicCounter) {
	t.Helper()
	deps, cfg, windowCalls := newSummarizeHarness(t, NewSummaryCache(16), 0.25)

	var fidelityCalls atomicCounter
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		fidelityCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + jsonQuote(fidelityBody) + `}}]}`))
	}))
	t.Cleanup(gate.Close)

	provider := &config.Provider{Name: "fidelity", URL: gate.URL, ApiFormat: "openai"}
	engine := &config.EngineRef{Provider: "fidelity", Model: "judge"}
	engine.ResolvedTargets = []config.EngineTarget{{
		Provider: provider,
		Engine:   config.EngineConfig{Provider: "fidelity", Model: "judge", TimeoutSeconds: 2},
	}}
	metrics := infra.NewMetrics()
	judge, err := NewJudge(SummaryFidelityContract(), engine, JudgeDeps{
		ClientFor:    func(string) *http.Client { return gate.Client() },
		InjectAPIKey: func(string, http.Header) error { return nil },
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:      metrics,
	})
	if err != nil {
		t.Fatalf("NewJudge: %v", err)
	}
	deps.FidelityGate = judge
	deps.FidelityGateStrict = strict
	deps.Metrics = metrics
	return deps, cfg, windowCalls, &fidelityCalls
}

// jsonQuote wraps a raw engine content string as a JSON string literal.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func fidelityMessages() []interface{} {
	return []interface{}{
		msg("user", "a"), msg("assistant", "b"),
		msg("user", "c"), msg("assistant", "d"),
	}
}

func TestSummaryFidelityGateSkippedOnCacheHit(t *testing.T) {
	deps, cfg, windowCalls, gateCalls := newFidelityHarness(t, `{"verdict":"faithful"}`, http.StatusOK, false)
	messages := fidelityMessages()

	payload1 := map[string]interface{}{"messages": messages}
	if _, err := ApplyWindowCompaction(context.Background(), deps, payload1, messages, 500, cfg, 100, func(map[string]interface{}) int { return 10 }); err != nil {
		t.Fatal(err)
	}
	payload2 := map[string]interface{}{"messages": messages}
	if _, err := ApplyWindowCompaction(context.Background(), deps, payload2, messages, 500, cfg, 100, func(map[string]interface{}) int { return 10 }); err != nil {
		t.Fatal(err)
	}

	if windowCalls.Load() != 1 || gateCalls.Load() != 1 {
		t.Fatalf("window calls = %d, gate calls = %d; want 1, 1 (gate must not run on cache hits)", windowCalls.Load(), gateCalls.Load())
	}
	if !payloadsEqual(payload1, payload2) {
		t.Error("cache stability broken under the fidelity gate")
	}
	// A faithful verdict records no fallback (sample lines carry a
	// label brace; HELP/TYPE headers are always emitted).
	out := &bytes.Buffer{}
	deps.Metrics.WritePrometheus(out)
	if strings.Contains(out.String(), "nenya_summary_gate_fallbacks_total{") {
		t.Error("faithful verdict must not record a gate fallback")
	}
}

func TestSummaryFidelityGateFailOpenOnError(t *testing.T) {
	deps, cfg, _, gateCalls := newFidelityHarness(t, "garbage, not json", http.StatusOK, false)
	messages := fidelityMessages()
	payload := map[string]interface{}{"messages": messages}
	applied, err := ApplyWindowCompaction(context.Background(), deps, payload, messages, 500, cfg, 100, func(map[string]interface{}) int { return 10 })
	if err != nil || !applied {
		t.Fatalf("applied = %v, err = %v; want compaction applied (fail-open)", applied, err)
	}
	if gateCalls.Load() != 1 {
		t.Fatalf("gate calls = %d, want 1", gateCalls.Load())
	}
	// Fail-open: the engine summary (not a truncation) is in the payload.
	if !strings.Contains(summaryFromPayload(t, payload), "summary-") {
		t.Errorf("expected engine summary kept, got %q", summaryFromPayload(t, payload))
	}
}

func TestSummaryFidelityGateInsufficientLogKeepsSummary(t *testing.T) {
	deps, cfg, _, _ := newFidelityHarness(t, `{"verdict":"insufficient"}`, http.StatusOK, false)
	messages := fidelityMessages()
	payload := map[string]interface{}{"messages": messages}
	applied, err := ApplyWindowCompaction(context.Background(), deps, payload, messages, 500, cfg, 100, func(map[string]interface{}) int { return 10 })
	if err != nil || !applied {
		t.Fatalf("applied = %v, err = %v; want compaction applied (log action keeps summary)", applied, err)
	}
	if !strings.Contains(summaryFromPayload(t, payload), "summary-") {
		t.Errorf("expected engine summary kept under log action, got %q", summaryFromPayload(t, payload))
	}
}

func TestSummaryFidelityGateStrictFallsBackToTruncation(t *testing.T) {
	deps, cfg, _, gateCalls := newFidelityHarness(t, `{"verdict":"insufficient"}`, http.StatusOK, true)
	messages := fidelityMessages()
	payload := map[string]interface{}{"messages": messages}
	applied, err := ApplyWindowCompaction(context.Background(), deps, payload, messages, 500, cfg, 100, func(map[string]interface{}) int { return 10 })
	if err != nil || !applied {
		t.Fatalf("applied = %v, err = %v; want compaction applied via truncation fallback", applied, err)
	}
	got := summaryFromPayload(t, payload)
	if strings.Contains(got, "summary-") {
		t.Errorf("expected truncated history (engine summary discarded), got %q", got)
	}
	assertFallbackMetric(t, deps.Metrics, "strict", 1)
	// The deterministic truncation body must be present (the payload
	// wraps it in a compaction header).
	if !strings.Contains(got, "user:\na") || !strings.Contains(got, "assistant:\nb") {
		t.Errorf("strict fallback summary %q does not carry the truncated history", got)
	}

	// The rejected summary must not have been cached: a second identical
	// request regenerates and consults the gate again.
	if gateCalls.Load() != 1 {
		t.Fatalf("gate calls after first request = %d, want 1", gateCalls.Load())
	}
	payload2 := map[string]interface{}{"messages": messages}
	if _, err := ApplyWindowCompaction(context.Background(), deps, payload2, messages, 500, cfg, 100, func(map[string]interface{}) int { return 10 }); err != nil {
		t.Fatal(err)
	}
	if gateCalls.Load() != 2 {
		t.Fatalf("gate calls after second request = %d, want 2 (no cache pollution from rejected summary)", gateCalls.Load())
	}
}

func TestSummaryFidelityGateInputCapsHistoryKeepsSummary(t *testing.T) {
	// The composed gate input must contain the full summary even when
	// the history excerpt is budget-capped.
	in := fidelityGateInput(strings.Repeat("h", 10000), strings.Repeat("s", 4000), 8192)
	if !strings.Contains(in, strings.Repeat("s", 4000)) {
		t.Error("summary was truncated out of the gate input")
	}
	if len(in) > 8192 {
		t.Errorf("gate input %d bytes exceeds budget 8192", len(in))
	}
}

func TestSummaryFidelityGateInputOverBudgetSummaryStaysBounded(t *testing.T) {
	// A summary that alone exceeds the budget must leave the composed
	// input within budget with a visible summary prefix (not tail-clipped
	// into oblivion by the Judge's re-cap).
	in := fidelityGateInput(strings.Repeat("h", 10000), strings.Repeat("s", 9000), 8192)
	if len(in) > 8192 {
		t.Fatalf("gate input %d bytes exceeds budget 8192", len(in))
	}
	if !strings.Contains(in, "SUMMARY:\n") || !strings.Contains(in, strings.Repeat("s", 64)) {
		t.Errorf("summary prefix missing from bounded input: %q", in[len(in)-200:])
	}
}

func summaryFromPayload(t *testing.T, payload map[string]interface{}) string {
	t.Helper()
	msgs, ok := payload["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		t.Fatal("payload has no messages")
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := mm["role"].(string); role == "system" {
			if c, ok := mm["content"].(string); ok && strings.Contains(c, "summary") {
				return c
			}
		}
	}
	// Fallback: the compacted head message carries the summary.
	if head, ok := msgs[0].(map[string]interface{}); ok {
		if c, ok := head["content"].(string); ok {
			return c
		}
	}
	return ""
}

func TestSummaryFidelityGateInconclusiveStrictKeepsSummary(t *testing.T) {
	deps, cfg, _, _ := newFidelityHarness(t, `{"verdict":"inconclusive"}`, http.StatusOK, true)
	messages := fidelityMessages()
	payload := map[string]interface{}{"messages": messages}
	applied, err := ApplyWindowCompaction(context.Background(), deps, payload, messages, 500, cfg, 100, func(map[string]interface{}) int { return 10 })
	if err != nil || !applied {
		t.Fatalf("applied = %v, err = %v; want compaction applied", applied, err)
	}
	if !strings.Contains(summaryFromPayload(t, payload), "summary-") {
		t.Errorf("inconclusive must keep the engine summary, got %q", summaryFromPayload(t, payload))
	}
	// Strict is reserved for actual rejects: an inconclusive keep is
	// labeled "log" so the metric stays truthful to its documentation.
	assertFallbackMetric(t, deps.Metrics, "log", 1)
}

func TestSummaryFidelityGateStrictFailureKeepsSummary(t *testing.T) {
	deps, cfg, _, _ := newFidelityHarness(t, `{"verdict":"faithful"}`, http.StatusServiceUnavailable, true)
	messages := fidelityMessages()
	payload := map[string]interface{}{"messages": messages}
	applied, err := ApplyWindowCompaction(context.Background(), deps, payload, messages, 500, cfg, 100, func(map[string]interface{}) int { return 10 })
	if err != nil || !applied {
		t.Fatalf("applied = %v, err = %v; want compaction applied (fail-open under strict)", applied, err)
	}
	if !strings.Contains(summaryFromPayload(t, payload), "summary-") {
		t.Errorf("operational failure must keep the engine summary, got %q", summaryFromPayload(t, payload))
	}
	assertFallbackMetric(t, deps.Metrics, "error", 1)
}

func TestSummaryFidelityGateTruncateModeNeverConsultsGate(t *testing.T) {
	deps, cfg, _, gateCalls := newFidelityHarness(t, `{"verdict":"insufficient"}`, http.StatusOK, true)
	cfg.Mode = "truncate"
	messages := fidelityMessages()
	payload := map[string]interface{}{"messages": messages}
	applied, err := ApplyWindowCompaction(context.Background(), deps, payload, messages, 500, cfg, 100, func(map[string]interface{}) int { return 10 })
	if err != nil || !applied {
		t.Fatalf("applied = %v, err = %v", applied, err)
	}
	if gateCalls.Load() != 0 {
		t.Fatalf("gate consulted %d times in truncate mode; want 0", gateCalls.Load())
	}
}

// assertFallbackMetric checks nenya_summary_gate_fallbacks_total via the
// Prometheus writer (line-wise family+label match).
func assertFallbackMetric(t *testing.T, metrics *infra.Metrics, action string, want uint64) {
	t.Helper()
	out := &bytes.Buffer{}
	metrics.WritePrometheus(out)
	wantLine := `action="` + action + `"`
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.Contains(line, "nenya_summary_gate_fallbacks_total{") || !strings.Contains(line, wantLine) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var got uint64
		if _, err := fmt.Sscanf(fields[len(fields)-1], "%d", &got); err == nil && got == want {
			return
		}
		t.Fatalf("fallback metric %s = %q, want %d", wantLine, fields[len(fields)-1], want)
	}
	t.Fatalf("nenya_summary_gate_fallbacks_total{%s} not found in output", wantLine)
}

func TestApplyFidelityGateTruncatedVerdictKeepsSummary(t *testing.T) {
	// A favorable verdict on a truncated excerpt is inconclusive for the
	// unseen tail: keep the summary and label the fallback "log", even
	// under strict action (only actual rejects may trigger strict).
	// The tiny MaxBytes forces excerpt truncation, making res.Truncated
	// true despite the faithful verdict.
	judge := newJudgeWithContract(t, JudgmentContract{
		Name: "summary_fidelity", System: "You are a summary fidelity judge.",
		Verdicts: []string{"faithful", "insufficient", "inconclusive"}, MaxBytes: 32,
	}, `{"verdict":"faithful"}`, http.StatusOK)
	metrics := infra.NewMetrics()
	judge.deps.Metrics = metrics
	deps := WindowDeps{
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		FidelityGateStrict: true,
		Metrics:            metrics,
		FidelityGate:       judge,
	}
	got := applyFidelityGate(context.Background(), deps, "inline", "the summary", strings.Repeat("h", 500))
	if got != "the summary" {
		t.Fatalf("truncated verdict must keep the summary, got %q", got)
	}
	assertFallbackMetric(t, metrics, "log", 1)
}
