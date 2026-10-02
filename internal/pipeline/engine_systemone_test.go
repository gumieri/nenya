package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
)

// systemOneJudgeStub builds a judge whose first target is a System One
// sidecar (api_format "systemone") and optionally a chat fallback. The
// stub records the decoded request body and answers with the given
// choice/confidence.
func systemOneJudgeStub(t *testing.T, choice string, confidence float64, withChatFallback bool) (*Judge, *atomic.Int32, *atomic.Pointer[map[string]any]) {
	t.Helper()
	var calls atomic.Int32
	var lastBody atomic.Pointer[map[string]any]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		calls.Add(1)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		lastBody.Store(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"verdict":{"type":"choice","choice":"` + choice + `","confidence":` + jsonNum(confidence) + `}}}`))
	}))
	t.Cleanup(server.Close)

	provider := &config.Provider{Name: "s1", URL: server.URL, ApiFormat: SystemOneAPIFormat}
	engine := &config.EngineRef{Provider: "s1", Model: "laya"}
	engine.ResolvedTargets = []config.EngineTarget{{
		Provider: provider,
		Engine:   config.EngineConfig{Provider: "s1", Model: "laya", TimeoutSeconds: 2},
	}}
	if withChatFallback {
		chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"verdict\":\"insufficient\"}"}}]}`))
		}))
		t.Cleanup(chat.Close)
		engine.ResolvedTargets = append(engine.ResolvedTargets, config.EngineTarget{
			Provider: &config.Provider{Name: "chat", URL: chat.URL, ApiFormat: "openai"},
			Engine:   config.EngineConfig{Provider: "chat", Model: "m", TimeoutSeconds: 2},
		})
	}
	judge, err := NewJudge(SummaryFidelityContract(), engine, JudgeDeps{
		ClientFor:    func(string) *http.Client { return server.Client() },
		InjectAPIKey: func(string, http.Header) error { return nil },
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:      infra.NewMetrics(),
	})
	if err != nil {
		t.Fatalf("NewJudge: %v", err)
	}
	return judge, &calls, &lastBody
}

// jsonNum renders a float without locale/format surprises.
func jsonNum(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func TestCallEngineSystemOneHappyPath(t *testing.T) {
	judge, calls, body := systemOneJudgeStub(t, "faithful", 0.9, false)
	res := judge.Adjudicate(context.Background(), "agent", "ok summary")
	if !res.OK || res.Verdict != "faithful" || res.Engine != "s1" {
		t.Fatalf("res = %+v", res)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
	b := *body.Load()
	if b["model"] != "laya" {
		t.Errorf("model = %v", b["model"])
	}
	qs, _ := b["questions"].(map[string]any)
	if len(qs) != 1 {
		t.Fatalf("questions = %v", b["questions"])
	}
	q, _ := qs["verdict"].(map[string]any)
	if q["type"] != "choice" {
		t.Errorf("question = %v", q)
	}
	criteria, _ := q["criteria"].(map[string]any)
	if len(criteria) == 0 {
		t.Errorf("criteria = %v", criteria)
	}
	if _, ok := criteria["faithful"]; !ok {
		t.Errorf("criteria missing enum member: %v", criteria)
	}
}

func TestCallEngineSystemOneStateCap(t *testing.T) {
	judge, _, body := systemOneJudgeStub(t, "faithful", 0.9, false)
	content := strings.Repeat("0123456789", 1000)
	res := judge.Adjudicate(context.Background(), "agent", content)
	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
	b := *body.Load()
	state, _ := b["state"].(string)
	if len(state) > SystemOneStateCapBytes {
		t.Errorf("state = %d bytes, cap %d", len(state), SystemOneStateCapBytes)
	}
}

func TestSystemOneLowConfidenceCascadesToChat(t *testing.T) {
	judge, _, _ := systemOneJudgeStub(t, "faithful", 0.3, true)
	judge.contract.EscalateBelowConfidence = 0.5
	res := judge.Adjudicate(context.Background(), "agent", "ok summary")
	// The chat fallback answers {"verdict":"insufficient"}.
	if !res.OK || res.Verdict != "insufficient" {
		t.Fatalf("res = %+v", res)
	}
	if res.Engine != "chat" {
		t.Errorf("engine = %q, want chat (cascade fall-through)", res.Engine)
	}
}

func TestSystemOneLowConfidenceNoFallbackFails(t *testing.T) {
	judge, _, _ := systemOneJudgeStub(t, "faithful", 0.3, false)
	judge.contract.EscalateBelowConfidence = 0.5
	res := judge.Adjudicate(context.Background(), "agent", "ok summary")
	if res.OK {
		t.Fatalf("low confidence without fallback must fail: %+v", res)
	}
}

func TestSystemOneNoThresholdAcceptsLowConfidence(t *testing.T) {
	judge, _, _ := systemOneJudgeStub(t, "faithful", 0.1, false)
	res := judge.Adjudicate(context.Background(), "agent", "ok summary")
	if !res.OK || res.Verdict != "faithful" || res.Engine != "s1" {
		t.Fatalf("res = %+v", res)
	}
}

func TestSystemOneMalformedAnswerFailsClosed(t *testing.T) {
	judge, _, _ := systemOneJudgeStub(t, "garbage-verdict", 1.0, false)
	res := judge.Adjudicate(context.Background(), "agent", "ok summary")
	if res.OK {
		t.Fatalf("out-of-enum choice must fail closed: %+v", res)
	}
}

func TestExtractSystemOneAnswer(t *testing.T) {
	q := &systemOneQuestion{ID: "verdict", Choices: []string{"a", "b"}}
	resp := &systemOneResponse{}
	if err := json.Unmarshal([]byte(`{"answers":{"other":{"choice":"a"},"verdict":{"type":"choice","choice":"b","confidence":0.8}}}`), resp); err != nil {
		t.Fatal(err)
	}
	choice, conf, err := extractSystemOneAnswer(resp, q)
	if err != nil || choice != "b" || conf != 0.8 {
		t.Fatalf("choice=%q conf=%v err=%v", choice, conf, err)
	}
	if _, _, err := extractSystemOneAnswer(&systemOneResponse{}, q); err == nil {
		t.Fatal("missing answer must error")
	}
}

func TestSystemOneExplicitZeroConfidenceCascades(t *testing.T) {
	// An explicit confidence:0 must not be coerced to 1 — it cascades
	// when a threshold is set (confidence routes, never decides policy).
	judge, _, _ := systemOneJudgeStub(t, "faithful", 0, false)
	judge.contract.EscalateBelowConfidence = 0.5
	if res := judge.Adjudicate(context.Background(), "agent", "ok summary"); res.OK {
		t.Fatalf("explicit zero confidence must cascade/fail: %+v", res)
	}
	// With the threshold off, an explicit zero is accepted.
	judge2, _, _ := systemOneJudgeStub(t, "faithful", 0, false)
	if res := judge2.Adjudicate(context.Background(), "agent", "ok summary"); !res.OK {
		t.Fatalf("threshold-off zero confidence must be accepted: %+v", res)
	}
}

func TestSystemOneAbsentConfidenceDefaultsToDecisive(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"answers":{"verdict":{"type":"choice","choice":"faithful"}}}`))
	}))
	t.Cleanup(server.Close)
	engine := &config.EngineRef{Provider: "s1", Model: "laya"}
	engine.ResolvedTargets = []config.EngineTarget{{
		Provider: &config.Provider{Name: "s1", URL: server.URL, ApiFormat: SystemOneAPIFormat},
		Engine:   config.EngineConfig{Provider: "s1", Model: "laya", TimeoutSeconds: 2},
	}}
	judge, err := NewJudge(SummaryFidelityContract(), engine, JudgeDeps{
		ClientFor:    func(string) *http.Client { return server.Client() },
		InjectAPIKey: func(string, http.Header) error { return nil },
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:      infra.NewMetrics(),
	})
	if err != nil {
		t.Fatal(err)
	}
	judge.contract.EscalateBelowConfidence = 0.5
	if res := judge.Adjudicate(context.Background(), "agent", "ok summary"); !res.OK {
		t.Fatalf("absent confidence defaults to decisive: %+v", res)
	}
}

func TestSystemOneStateCapReportsTruncated(t *testing.T) {
	// Content within the contract excerpt budget but over the 2048-byte
	// System One state window must report Truncated (the tail was never
	// adjudicated).
	judge, _, _ := systemOneJudgeStub(t, "faithful", 1.0, false)
	judge.contract = judge.contract.WithBudget(4096, 0)
	content := strings.Repeat("x", 3000)
	res := judge.Adjudicate(context.Background(), "agent", content)
	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
	if !res.Truncated {
		t.Fatal("state-cap cut must report Truncated")
	}
}

func TestSystemOnePermanent4xxNotRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"bad question"}`))
	}))
	t.Cleanup(server.Close)
	engine := &config.EngineRef{Provider: "s1", Model: "laya"}
	engine.ResolvedTargets = []config.EngineTarget{{
		Provider: &config.Provider{Name: "s1", URL: server.URL, ApiFormat: SystemOneAPIFormat, MaxRetryAttempts: 3},
		Engine:   config.EngineConfig{Provider: "s1", Model: "laya", TimeoutSeconds: 2},
	}}
	judge, err := NewJudge(SummaryFidelityContract(), engine, JudgeDeps{
		ClientFor:    func(string) *http.Client { return server.Client() },
		InjectAPIKey: func(string, http.Header) error { return nil },
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:      infra.NewMetrics(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := judge.Adjudicate(context.Background(), "agent", "ok summary"); res.OK {
		t.Fatalf("4xx must fail: %+v", res)
	}
	if calls.Load() != 1 {
		t.Fatalf("permanent 4xx calls = %d, want 1 (no retry)", calls.Load())
	}
}

func TestSystemOneMapContractTargetRejected(t *testing.T) {
	// A map contract (tfidf_rerank) with a systemone target: the typed
	// transport is unavailable, the target errors, and the deterministic
	// pruning stands (judgment not OK).
	judge, calls, _ := systemOneJudgeStub(t, "faithful", 1.0, false)
	_, res := judge.AdjudicateMap(context.Background(), "agent", "body",
		func(string) (map[int]bool, error) { return nil, nil }, "relevant")
	if res.OK {
		t.Fatalf("map contract must not accept a systemone verdict: %+v", res)
	}
	if calls.Load() != 0 {
		t.Fatalf("systemone endpoint must not be called for map contracts: %d", calls.Load())
	}
}

func TestSystemOneStateCapNotAppliedWhenChatAnswers(t *testing.T) {
	// A chat fallback that answers examines the full excerpt; the
	// System One state cap must not mark its verdict truncated.
	judge, _, _ := systemOneJudgeStub(t, "faithful", 0.3, true)
	judge.contract = judge.contract.WithBudget(4096, 0)
	judge.contract.EscalateBelowConfidence = 0.5
	res := judge.Adjudicate(context.Background(), "agent", strings.Repeat("x", 3000))
	if !res.OK || res.Engine != "chat" {
		t.Fatalf("res = %+v", res)
	}
	if res.Truncated {
		t.Fatal("chat fallback must not inherit the systemone state-cap truncation")
	}
}
