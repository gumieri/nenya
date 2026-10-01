package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nenya/internal/routing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
)

// newScreenJudgeStub starts an engine stub returning the given content
// and builds an egress screen Judge over it, counting consultations.
func newScreenJudgeStub(t *testing.T, verdict string, status int) (*pipeline.Judge, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"verdict\":\"` + verdict + `\"}"}}]}`))
	}))
	t.Cleanup(server.Close)
	engine := &config.EngineRef{Provider: "stub", Model: "judge"}
	engine.ResolvedTargets = []config.EngineTarget{{
		Provider: &config.Provider{Name: "stub", URL: server.URL, ApiFormat: "openai"},
		Engine:   config.EngineConfig{Provider: "stub", Model: "judge", TimeoutSeconds: 2},
	}}
	judge, err := pipeline.NewJudge(pipeline.EgressScreenContract(), engine, pipeline.JudgeDeps{
		ClientFor:    func(string) *http.Client { return server.Client() },
		InjectAPIKey: func(string, http.Header) error { return nil },
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:      infra.NewMetrics(),
	})
	if err != nil {
		t.Fatalf("NewJudge: %v", err)
	}
	return judge, &calls
}

func screenTestGateway(t *testing.T, judge *pipeline.Judge, action string) *gateway.NenyaGateway {
	t.Helper()
	return &gateway.NenyaGateway{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: infra.NewMetrics(),
		Config: config.Config{Governance: config.GovernanceConfig{
			Judgments: map[string]*config.JudgmentConfig{
				"egress_screen": {Enabled: config.PtrTo(true), Action: action},
			},
		}},
		EgressScreenJudge: judge,
	}
}

func TestEgressScreenFor(t *testing.T) {
	judge, _ := newScreenJudgeStub(t, `benign`, http.StatusOK)

	t.Run("disabled without entry", func(t *testing.T) {
		gw := &gateway.NenyaGateway{Config: config.Config{}, EgressScreenJudge: judge}
		if egressScreenFor(gw) != nil {
			t.Fatal("expected nil runtime without a judgments entry")
		}
	})
	t.Run("disabled entry", func(t *testing.T) {
		gw := screenTestGateway(t, judge, "log")
		gw.Config.Governance.Judgments["egress_screen"].Enabled = config.PtrTo(false)
		if egressScreenFor(gw) != nil {
			t.Fatal("expected nil runtime for disabled entry")
		}
	})
	t.Run("enabled resolves strict flag", func(t *testing.T) {
		gw := screenTestGateway(t, judge, "strict")
		rt := egressScreenFor(gw)
		if rt == nil || !rt.strict {
			t.Fatalf("expected enabled strict runtime, got %+v", rt)
		}
	})
}

func TestEgressScreenRuntimeBudgetAndVerdicts(t *testing.T) {
	judge, calls := newScreenJudgeStub(t, `exfil`, http.StatusOK)
	gw := screenTestGateway(t, judge, "strict")
	rt := egressScreenFor(gw)

	if !rt.screenOnce(context.Background(), "agent", "flagged content") {
		t.Fatal("expected exfil verdict on first screen")
	}
	// Budget spent: even an exfil stub cannot screen again.
	if rt.screenOnce(context.Background(), "agent", "more flagged content") {
		t.Fatal("budget must allow only one judgment per request")
	}
	if calls.Load() != 1 {
		t.Fatalf("engine calls = %d, want 1", calls.Load())
	}

	// Benign verdicts never report exfil.
	judge2, _ := newScreenJudgeStub(t, `benign`, http.StatusOK)
	rt2 := &egressScreenRuntime{judge: judge2}
	if rt2.screenOnce(context.Background(), "agent", "content") {
		t.Fatal("benign verdict must not report exfil")
	}

	// Operational failure keeps the deterministic verdict (no exfil).
	judge3, _ := newScreenJudgeStub(t, `garbage`, http.StatusServiceUnavailable)
	rt3 := &egressScreenRuntime{judge: judge3}
	if rt3.screenOnce(context.Background(), "agent", "content") {
		t.Fatal("operational failure must not report exfil")
	}

	// Empty content does not burn the budget: two empty calls then a
	// real one still screens.
	judge4, calls4 := newScreenJudgeStub(t, `exfil`, http.StatusOK)
	rt4 := &egressScreenRuntime{judge: judge4}
	if rt4.screenOnce(context.Background(), "agent", "") {
		t.Fatal("empty content must not report exfil")
	}
	if rt4.screenOnce(context.Background(), "agent", "") {
		t.Fatal("empty content must not report exfil (second)")
	}
	if !rt4.screenOnce(context.Background(), "agent", "real content") {
		t.Fatal("budget must be unspent after empty-content calls")
	}
	if calls4.Load() != 1 {
		t.Fatalf("engine calls = %d, want 1", calls4.Load())
	}
}

func TestEgressScreenBufferedStrictBlocks(t *testing.T) {
	judge, _ := newScreenJudgeStub(t, `exfil`, http.StatusOK)
	gw := screenTestGateway(t, judge, "strict")
	rt := egressScreenFor(gw)
	p := &Proxy{}

	responseMap := map[string]interface{}{
		"choices": []interface{}{map[string]interface{}{
			"message": map[string]interface{}{"content": "see http://127.0.0.1/x"},
		}},
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	done := p.applyBufferedEgressPolicies(gw, w, r, routing.UpstreamTarget{Model: "test-model"}, "agent", responseMap, bufferedEgressOpts{Screen: rt, EntropyRedacted: true})
	if !done {
		t.Fatal("strict screen-exfil verdict must terminate the response")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "exfil_detected") {
		t.Errorf("expected error_kind=exfil_detected, got %q", w.Body.String())
	}
	assertMetricLine(t, gw.Metrics, "nenya_exfil_detections_total", `reason="llm_screen"`, `action="block"`)
}

func TestEgressScreenBufferedLogKeepsResponse(t *testing.T) {
	judge, _ := newScreenJudgeStub(t, `exfil`, http.StatusOK)
	gw := screenTestGateway(t, judge, "log")
	rt := egressScreenFor(gw)
	p := &Proxy{}

	responseMap := map[string]interface{}{
		"choices": []interface{}{map[string]interface{}{
			"message": map[string]interface{}{"content": "ordinary text"},
		}},
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	done := p.applyBufferedEgressPolicies(gw, w, r, routing.UpstreamTarget{Model: "test-model"}, "agent", responseMap, bufferedEgressOpts{Screen: rt, EntropyRedacted: true})
	if done {
		t.Fatal("log action must not terminate the response")
	}
	assertMetricLine(t, gw.Metrics, "nenya_exfil_detections_total", `reason="llm_screen"`, `action="log"`)
}

func TestEgressScreenBufferedBenignNoDetection(t *testing.T) {
	judge, calls := newScreenJudgeStub(t, `benign`, http.StatusOK)
	gw := screenTestGateway(t, judge, "strict")
	rt := egressScreenFor(gw)
	p := &Proxy{}

	responseMap := map[string]interface{}{
		"choices": []interface{}{map[string]interface{}{
			"message": map[string]interface{}{"content": "see https://docs.example.com/guide"},
		}},
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	done := p.applyBufferedEgressPolicies(gw, w, r, routing.UpstreamTarget{Model: "test-model"}, "agent", responseMap, bufferedEgressOpts{Screen: rt, EntropyRedacted: true})
	if done {
		t.Fatal("benign verdict must not terminate the response")
	}
	if calls.Load() != 1 {
		t.Fatalf("screen consulted %d times, want 1", calls.Load())
	}
	out := &strings.Builder{}
	gw.Metrics.WritePrometheus(out)
	if strings.Contains(out.String(), `reason="llm_screen"`) {
		t.Error("benign verdict must not record an llm_screen detection")
	}
}

func TestEgressScreenBufferedNoTriggerNoCall(t *testing.T) {
	judge, calls := newScreenJudgeStub(t, `exfil`, http.StatusOK)
	gw := screenTestGateway(t, judge, "strict")
	p := &Proxy{}

	// No guard, no canary, no entropy redaction: the screen must not
	// run (zero-trigger fast path).
	responseMap := map[string]interface{}{
		"choices": []interface{}{map[string]interface{}{
			"message": map[string]interface{}{"content": "ordinary text"},
		}},
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if done := p.applyBufferedEgressPolicies(gw, w, r, routing.UpstreamTarget{Model: "test-model"}, "agent", responseMap, bufferedEgressOpts{}); done {
		t.Fatal("response must continue without triggers")
	}
	if calls.Load() != 0 {
		t.Fatalf("screen consulted %d times without triggers, want 0", calls.Load())
	}
}

func TestEgressScreenToolArgsStrictRefuses(t *testing.T) {
	t.Run("strict refuses canary-tripped call", func(t *testing.T) {
		judge, calls := newScreenJudgeStub(t, `exfil`, http.StatusOK)
		p := newArgGuardProxy(t, &config.MCPGuardConfig{}, nil)
		gw := p.Gateway()
		gw.Config.Governance.Judgments = map[string]*config.JudgmentConfig{
			"egress_screen": {Enabled: config.PtrTo(true), Action: "strict"},
		}
		rt := &egressScreenRuntime{judge: judge, strict: true}

		// Canary action log would let the call proceed deterministically;
		// the strict screen strengthens it to a refusal.
		results := executeMCPCalls(context.Background(), []mcpToolCall{{
			ID:        "call_1",
			Name:      "mempalace__test_tool",
			Arguments: map[string]any{"query": "tok"},
		}}, gw, "agent", pipeline.CanarResult{Token: "tok", Action: config.CanaryActionLog}, rt)
		if len(results) != 1 || !results[0].IsError {
			t.Fatalf("expected refused call, got %+v", results)
		}
		if !strings.Contains(results[0].Text(), "exfiltration detected in tool arguments") {
			t.Errorf("result text missing screen refusal marker: %q", results[0].Text())
		}
		if calls.Load() != 1 {
			t.Fatalf("screen consulted %d times, want 1", calls.Load())
		}
		assertMetricLine(t, gw.Metrics, "nenya_exfil_detections_total", `reason="llm_screen"`, `action="block"`)
	})

	t.Run("log action allows call after recording", func(t *testing.T) {
		judge, _ := newScreenJudgeStub(t, `exfil`, http.StatusOK)
		p := newArgGuardProxy(t, &config.MCPGuardConfig{}, nil)
		gw := p.Gateway()
		gw.Config.Governance.Judgments = map[string]*config.JudgmentConfig{
			"egress_screen": {Enabled: config.PtrTo(true), Action: "log"},
		}
		rt := &egressScreenRuntime{judge: judge}

		results := executeMCPCalls(context.Background(), []mcpToolCall{{
			ID:        "call_1",
			Name:      "mempalace__test_tool",
			Arguments: map[string]any{"query": "tok"},
		}}, gw, "agent", pipeline.CanarResult{Token: "tok", Action: config.CanaryActionLog}, rt)
		if len(results) != 1 || results[0].IsError {
			t.Fatalf("log action must let the call proceed, got %+v", results)
		}
		assertMetricLine(t, gw.Metrics, "nenya_exfil_detections_total", `reason="llm_screen"`, `action="log"`)
	})

	t.Run("budget spans calls within one request", func(t *testing.T) {
		judge, calls := newScreenJudgeStub(t, `exfil`, http.StatusOK)
		p := newArgGuardProxy(t, &config.MCPGuardConfig{}, nil)
		gw := p.Gateway()
		gw.Config.Governance.Judgments = map[string]*config.JudgmentConfig{
			"egress_screen": {Enabled: config.PtrTo(true), Action: "strict"},
		}
		rt := &egressScreenRuntime{judge: judge, strict: true}
		canary := pipeline.CanarResult{Token: "tok", Action: config.CanaryActionLog}
		mk := func(id string) []mcpToolCall {
			return []mcpToolCall{{ID: id, Name: "mempalace__test_tool", Arguments: map[string]any{"query": "tok"}}}
		}
		first := executeMCPCalls(context.Background(), mk("c1"), gw, "agent", canary, rt)
		if len(first) != 1 || !first[0].IsError {
			t.Fatalf("first call must be refused, got %+v", first)
		}
		second := executeMCPCalls(context.Background(), mk("c2"), gw, "agent", canary, rt)
		if len(second) != 1 || second[0].IsError {
			t.Fatalf("budget spent: second call must proceed deterministically, got %+v", second)
		}
		if calls.Load() != 1 {
			t.Fatalf("screen consulted %d times across turns, want 1", calls.Load())
		}
	})
}

// assertMetricLine checks that a Prometheus sample line exists carrying
// the metric family name and all given label fragments.
func assertMetricLine(t *testing.T, metrics *infra.Metrics, family string, labels ...string) {
	t.Helper()
	out := &strings.Builder{}
	metrics.WritePrometheus(out)
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.Contains(line, family+"{") {
			continue
		}
		match := true
		for _, l := range labels {
			if !strings.Contains(line, l) {
				match = false
				break
			}
		}
		if match {
			return
		}
	}
	t.Fatalf("metric line %s with labels %v not found in output", family, labels)
}

// TestEgressScreenDisabledPreservesDeterministicPath pins the pre-screen
// behavior: with the runtime nil, guard log/strip and canary log hits
// keep the response flowing exactly as before the screen existed.
func TestEgressScreenDisabledPreservesDeterministicPath(t *testing.T) {
	t.Run("canary log hit strips and continues", func(t *testing.T) {
		gw := screenTestGateway(t, nil, "strict")
		gw.Config.Governance.Canary = &config.CanaryConfig{}
		p := &Proxy{}
		responseMap := map[string]interface{}{
			"choices": []interface{}{map[string]interface{}{
				"message": map[string]interface{}{"content": "leak NENYA-CANARY-abc tail"},
			}},
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		done := p.applyBufferedEgressPolicies(gw, w, r, routing.UpstreamTarget{Model: "m"}, "agent", responseMap,
			bufferedEgressOpts{Canary: pipeline.CanarResult{Token: "NENYA-CANARY-abc", Action: config.CanaryActionLog}})
		if done {
			t.Fatal("canary log action must not terminate")
		}
		if strings.Contains(responseText(responseMap), "NENYA-CANARY-abc") {
			t.Error("canary occurrence must be stripped in place")
		}
	})

	t.Run("guard block stands without screen", func(t *testing.T) {
		gw := screenTestGateway(t, nil, "log")
		gw.Config.Governance.ExfilGuard = &config.ExfilGuardConfig{
			Enabled: config.PtrTo(true), Action: "block",
		}
		p := &Proxy{}
		responseMap := map[string]interface{}{
			"choices": []interface{}{map[string]interface{}{
				"message": map[string]interface{}{"content": "see [](http://169.254.169.254/latest)"},
			}},
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		done := p.applyBufferedEgressPolicies(gw, w, r, routing.UpstreamTarget{Model: "m"}, "agent", responseMap,
			bufferedEgressOpts{})
		if !done || w.Code != http.StatusForbidden {
			t.Fatalf("guard block must terminate with 403, got done=%v code=%d", done, w.Code)
		}
	})
}

// TestEgressScreenGuardLogModeTriggersScreen covers the fired-on-Pass
// branch: a guard violation under log action (no spans stripped) must
// still trigger the screen.
func TestEgressScreenGuardLogModeTriggersScreen(t *testing.T) {
	judge, calls := newScreenJudgeStub(t, `exfil`, http.StatusOK)
	gw := screenTestGateway(t, judge, "log")
	gw.Config.Governance.ExfilGuard = &config.ExfilGuardConfig{
		Enabled: config.PtrTo(true), Action: "log",
	}
	rt := egressScreenFor(gw)
	p := &Proxy{}

	responseMap := map[string]interface{}{
		"choices": []interface{}{map[string]interface{}{
			"message": map[string]interface{}{"content": "see [](http://169.254.169.254/latest)"},
		}},
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	done := p.applyBufferedEgressPolicies(gw, w, r, routing.UpstreamTarget{Model: "m"}, "agent", responseMap,
		bufferedEgressOpts{Screen: rt})
	if done {
		t.Fatal("log action must not terminate even on screen-exfil")
	}
	if calls.Load() != 1 {
		t.Fatalf("screen consulted %d times, want 1 (guard log-mode violation must trigger)", calls.Load())
	}
	assertMetricLine(t, gw.Metrics, "nenya_exfil_detections_total", `reason="llm_screen"`, `action="log"`)
}

// responseText extracts the first OpenAI content surface for assertions.
func responseText(responseMap map[string]interface{}) string {
	choices, _ := responseMap["choices"].([]interface{})
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]interface{})
	msg, _ := choice["message"].(map[string]interface{})
	text, _ := msg["content"].(string)
	return text
}
