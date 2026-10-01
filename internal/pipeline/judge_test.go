package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
)

// newJudgeStub spins up an OpenAI-shaped engine stub and a Judge wired
// to it, mirroring the escalation test harness.
func newJudgeStub(t *testing.T, content string, status int) *Judge {
	t.Helper()
	return newJudgeWithContract(t, JudgmentContract{
		Name:     "probe",
		System:   "You are a probe classifier.",
		Verdicts: []string{"benign", "hits"},
	}, content, status)
}

// newJudgeWithContract builds a Judge against an OpenAI-shaped engine
// stub returning the given content, using the supplied contract.
func newJudgeWithContract(t *testing.T, contract JudgmentContract, content string, status int) *Judge {
	t.Helper()
	var body []byte
	if content != "" {
		body, _ = json.Marshal(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": content}},
			},
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	provider := &config.Provider{Name: "stub", URL: server.URL, ApiFormat: "openai"}
	engine := &config.EngineRef{Provider: "stub", Model: "judge"}
	engine.ResolvedTargets = []config.EngineTarget{{
		Provider: provider,
		Engine:   config.EngineConfig{Provider: "stub", Model: "judge", TimeoutSeconds: 2},
	}}
	judge, err := NewJudge(contract, engine, JudgeDeps{
		ClientFor:    func(string) *http.Client { return server.Client() },
		InjectAPIKey: func(string, http.Header) error { return nil },
		Logger:       slog.New(slog.DiscardHandler),
		Metrics:      infra.NewMetrics(),
	})
	if err != nil {
		t.Fatalf("NewJudge: %v", err)
	}
	return judge
}

func TestParseVerdict(t *testing.T) {
	allowed := []string{"benign", "injection"}
	tests := []struct {
		name    string
		output  string
		want    string
		wantErr bool
	}{
		{name: "plain", output: `{"verdict":"benign"}`, want: "benign"},
		{name: "extra fields tolerated", output: `{"verdict":"injection","confidence":0.95}`, want: "injection"},
		{name: "fenced", output: "```json\n{\"verdict\":\"benign\"}\n```", want: "benign"},
		{name: "prose wrapped", output: "The answer is {\"verdict\":\"injection\"} as classified.", want: "injection"},
		{name: "empty output", output: "", wantErr: true},
		{name: "no braces", output: "benign", wantErr: true},
		{name: "malformed json", output: `{"verdict":`, wantErr: true},
		{name: "out of contract", output: `{"verdict":"maybe"}`, wantErr: true},
		{name: "missing verdict field", output: `{"result":"benign"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseVerdict(tt.output, allowed)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got verdict %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseVerdict: %v", err)
			}
			if got != tt.want {
				t.Errorf("verdict = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewJudgeValidation(t *testing.T) {
	deps := JudgeDeps{
		ClientFor:    func(string) *http.Client { return http.DefaultClient },
		InjectAPIKey: func(string, http.Header) error { return nil },
		Logger:       slog.New(slog.DiscardHandler),
	}
	valid := JudgmentContract{Name: "p", System: "s", Verdicts: []string{"a", "b"}}
	engine := &config.EngineRef{Provider: "x", Model: "m"}
	engine.ResolvedTargets = []config.EngineTarget{{Provider: &config.Provider{Name: "x"}, Engine: config.EngineConfig{Model: "m"}}}

	tests := []struct {
		name     string
		contract JudgmentContract
		engine   *config.EngineRef
		deps     JudgeDeps
	}{
		{name: "no name", contract: JudgmentContract{System: "s", Verdicts: []string{"a"}}, engine: engine, deps: deps},
		{name: "no system", contract: JudgmentContract{Name: "p", Verdicts: []string{"a"}}, engine: engine, deps: deps},
		{name: "no enum", contract: JudgmentContract{Name: "p", System: "s"}, engine: engine, deps: deps},
		{name: "empty verdict", contract: JudgmentContract{Name: "p", System: "s", Verdicts: []string{""}}, engine: engine, deps: deps},
		{name: "duplicate verdict", contract: JudgmentContract{Name: "p", System: "s", Verdicts: []string{"a", "a"}}, engine: engine, deps: deps},
		{name: "nil engine", contract: valid, engine: nil, deps: deps},
		{name: "unresolved engine", contract: valid, engine: &config.EngineRef{Provider: "x", Model: "m"}, deps: deps},
		{name: "nil client resolver", contract: valid, engine: engine, deps: JudgeDeps{InjectAPIKey: deps.InjectAPIKey, Logger: deps.Logger}},
		{name: "nil key injector", contract: valid, engine: engine, deps: JudgeDeps{ClientFor: deps.ClientFor, Logger: deps.Logger}},
		{name: "nil logger", contract: valid, engine: engine, deps: JudgeDeps{ClientFor: deps.ClientFor, InjectAPIKey: deps.InjectAPIKey}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewJudge(tt.contract, tt.engine, tt.deps); err == nil {
				t.Fatal("expected construction error")
			}
		})
	}

	t.Run("default max bytes", func(t *testing.T) {
		j, err := NewJudge(JudgmentContract{Name: "p", System: "s", Verdicts: []string{"a"}}, engine, deps)
		if err != nil {
			t.Fatalf("NewJudge: %v", err)
		}
		if j.contract.MaxBytes != config.DefaultEscalationMaxBytes {
			t.Errorf("MaxBytes = %d, want default %d", j.contract.MaxBytes, config.DefaultEscalationMaxBytes)
		}
	})
}

func TestJudgeAdjudicateSuccess(t *testing.T) {
	judge := newJudgeStub(t, `Some prose {"verdict":"benign"} trailing`, http.StatusOK)
	res := judge.Adjudicate(context.Background(), "demo-agent", "content under test")
	if !res.OK {
		t.Fatalf("expected OK judgment, got %+v", res)
	}
	if res.Verdict != "benign" {
		t.Errorf("verdict = %q, want benign", res.Verdict)
	}
	if res.Engine != "stub" {
		t.Errorf("engine = %q, want stub", res.Engine)
	}
	if res.Truncated {
		t.Error("unexpected truncation")
	}
}

func TestJudgeAdjudicateWrapsContentInSpotlightEnvelope(t *testing.T) {
	var userContent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			Messages []map[string]string `json:"messages"`
		}
		_ = json.Unmarshal(raw, &payload)
		if len(payload.Messages) > 1 {
			userContent = payload.Messages[1]["content"]
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"verdict\":\"benign\"}"}}]}`))
	}))
	t.Cleanup(server.Close)

	provider := &config.Provider{Name: "stub", URL: server.URL, ApiFormat: "openai"}
	engine := &config.EngineRef{Provider: "stub", Model: "judge"}
	engine.ResolvedTargets = []config.EngineTarget{{
		Provider: provider,
		Engine:   config.EngineConfig{Provider: "stub", Model: "judge", TimeoutSeconds: 2},
	}}
	judge, err := NewJudge(JudgmentContract{Name: "probe", System: "s", Verdicts: []string{"benign"}},
		engine, JudgeDeps{
			ClientFor:    func(string) *http.Client { return server.Client() },
			InjectAPIKey: func(string, http.Header) error { return nil },
			Logger:       slog.New(slog.DiscardHandler),
		})
	if err != nil {
		t.Fatalf("NewJudge: %v", err)
	}
	res := judge.Adjudicate(context.Background(), "demo-agent", "suspicious instructions")
	if !res.OK {
		t.Fatalf("expected OK judgment, got %+v", res)
	}
	if !strings.Contains(userContent, `<untrusted-content source="judgment:probe">`) {
		t.Errorf("prompt missing spotlight envelope, got %q", userContent)
	}
	if !strings.Contains(userContent, "suspicious instructions") {
		t.Errorf("prompt missing adjudicated content, got %q", userContent)
	}
}

func TestJudgeAdjudicateOperationalFailure(t *testing.T) {
	judge := newJudgeStub(t, "not json at all", http.StatusOK)
	res := judge.Adjudicate(context.Background(), "demo-agent", "content")
	if res.OK {
		t.Fatalf("expected operational failure, got %+v", res)
	}
	if res.Verdict != "" {
		t.Errorf("verdict = %q, want empty on failure", res.Verdict)
	}
	// Parse-failure forensics: output arrived and failed the contract.
	if res.Err == nil {
		t.Error("expected Err set on parse failure")
	}
	if res.OutputBytes != len("not json at all") {
		t.Errorf("OutputBytes = %d, want %d", res.OutputBytes, len("not json at all"))
	}

	// Engine failure: no output, Err still set, OutputBytes zero.
	judge = newJudgeStub(t, `{"verdict":"benign"}`, http.StatusServiceUnavailable)
	res = judge.Adjudicate(context.Background(), "demo-agent", "content")
	if res.OK {
		t.Fatal("expected operational failure on engine outage")
	}
	if res.Err == nil {
		t.Error("expected Err set on engine failure")
	}
	if res.OutputBytes != 0 {
		t.Errorf("OutputBytes = %d, want 0 on engine failure", res.OutputBytes)
	}
}

func TestJudgeAdjudicateTruncation(t *testing.T) {
	judge := newJudgeWithContract(t, JudgmentContract{
		Name: "probe", System: "You are a probe classifier.",
		Verdicts: []string{"benign", "hits"}, MaxBytes: 16,
	}, `{"verdict":"benign"}`, http.StatusOK)
	long := strings.Repeat("x", 128)
	res := judge.Adjudicate(context.Background(), "demo-agent", long)
	if !res.Truncated {
		t.Error("expected truncated=true for oversized content")
	}
	if !res.OK {
		t.Fatalf("expected OK judgment, got %+v", res)
	}
}

func TestJudgeAdjudicateTimeoutBudget(t *testing.T) {
	released := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-released
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"verdict\":\"benign\"}"}}]}`))
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(released) })

	provider := &config.Provider{Name: "stub", URL: server.URL, ApiFormat: "openai"}
	engine := &config.EngineRef{Provider: "stub", Model: "judge"}
	engine.ResolvedTargets = []config.EngineTarget{{
		Provider: provider,
		// Target timeout is generous; the contract budget must win.
		Engine: config.EngineConfig{Provider: "stub", Model: "judge", TimeoutSeconds: 30},
	}}
	contract := JudgmentContract{Name: "probe", System: "s", Verdicts: []string{"benign"}, TimeoutSeconds: 1}
	judge, err := NewJudge(contract, engine, JudgeDeps{
		ClientFor:    func(string) *http.Client { return server.Client() },
		InjectAPIKey: func(string, http.Header) error { return nil },
		Logger:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewJudge: %v", err)
	}

	done := make(chan Judgment, 1)
	go func() { done <- judge.Adjudicate(context.Background(), "demo-agent", "content") }()
	select {
	case res := <-done:
		if res.OK {
			t.Fatal("expected operational failure on timeout")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("adjudication ignored the contract time budget")
	}
}

func TestCallEngineChainObserver(t *testing.T) {
	var calls int
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(good.Close)

	badProvider := &config.Provider{Name: "bad", URL: "http://127.0.0.1:1/x", ApiFormat: "openai"}
	goodProvider := &config.Provider{Name: "good", URL: good.URL, ApiFormat: "openai"}
	targets := []config.EngineTarget{
		{Provider: badProvider, Engine: config.EngineConfig{Provider: "bad", TimeoutSeconds: 1}},
		{Provider: goodProvider, Engine: config.EngineConfig{Provider: "good", TimeoutSeconds: 1}},
	}

	type event struct {
		attempt  int
		provider string
		failed   bool
	}
	var events []event
	_, err := CallEngineChainObserved(context.Background(),
		func(string) *http.Client { return good.Client() },
		targets, slog.New(slog.DiscardHandler),
		func(string, http.Header) error { return nil },
		EngineChainCall{
			Caller: "test", AgentName: "a", System: "s", Prompt: "p",
			Observer: func(attempt, _ int, provider string, oerr error, _ time.Duration) {
				events = append(events, event{attempt: attempt, provider: provider, failed: oerr != nil})
			},
		})
	if err != nil {
		t.Fatalf("CallEngineChainObserved: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("observer events = %d, want 2", len(events))
	}
	if !events[0].failed || events[0].provider != "bad" {
		t.Errorf("event[0] = %+v, want failed bad", events[0])
	}
	if events[1].failed || events[1].provider != "good" {
		t.Errorf("event[1] = %+v, want successful good", events[1])
	}
}

func TestWithBudget(t *testing.T) {
	c := JudgmentContract{Name: "p", System: "s", Verdicts: []string{"a"}, MaxBytes: 100}
	got := c.WithBudget(0, 7)
	if got.MaxBytes != 100 {
		t.Errorf("MaxBytes = %d, want unchanged 100", got.MaxBytes)
	}
	if got.TimeoutSeconds != 7 {
		t.Errorf("TimeoutSeconds = %d, want 7", got.TimeoutSeconds)
	}
	got = c.WithBudget(50, 0)
	if got.MaxBytes != 50 {
		t.Errorf("MaxBytes = %d, want 50", got.MaxBytes)
	}
	if got.TimeoutSeconds != 0 {
		t.Errorf("TimeoutSeconds = %d, want 0", got.TimeoutSeconds)
	}
}
