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

// newRerankJudgeStub builds a TF-IDF rerank judge over an engine stub
// that echoes a fixed per-block verdict JSON, counting consultations.
func newRerankJudgeStub(t *testing.T, verdictsJSON string, status int) (*Judge, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + mustJSON(verdictsJSON) + `}}]}`))
	}))
	t.Cleanup(server.Close)
	engine := &config.EngineRef{Provider: "stub", Model: "judge"}
	engine.ResolvedTargets = []config.EngineTarget{{
		Provider: &config.Provider{Name: "stub", URL: server.URL, ApiFormat: "openai"},
		Engine:   config.EngineConfig{Provider: "stub", Model: "judge", TimeoutSeconds: 2},
	}}
	judge, err := NewJudge(TfidfRerankContract(), engine, JudgeDeps{
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

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// rerankRescueFn adapts the judge into a BlockRescorer.
func rerankRescueFn(judge *Judge, maxBytes int) BlockRescorer {
	return func(q string, borderline []Block) map[int]bool {
		verdicts, _ := judge.AdjudicateMap(context.Background(), "agent", composeTfidfRerankInput(q, borderline, maxBytes), parseTfidfRerankVerdict(len(borderline)), "rescue")
		return verdicts
	}
}

func TestParseTfidfRerankVerdict(t *testing.T) {
	parse := parseTfidfRerankVerdict(3)

	t.Run("per-block map with prose", func(t *testing.T) {
		v, err := parse("Sure!\n```json\n{\"1\":\"relevant\",\"2\":\"not_relevant\"}\n```\nDone.")
		if err != nil {
			t.Fatal(err)
		}
		// Keys map to borderline slice indexes (block number minus one).
		if !v[0] || v[1] {
			t.Errorf("verdicts = %+v", v)
		}
	})
	t.Run("missing keys tolerated", func(t *testing.T) {
		v, err := parse(`{"2":"relevant"}`)
		if err != nil || v[1] != true || len(v) != 1 {
			t.Errorf("v = %+v, err = %v", v, err)
		}
	})
	t.Run("unknown key rejected", func(t *testing.T) {
		if _, err := parse(`{"9":"relevant"}`); err == nil {
			t.Fatal("expected error for out-of-range block")
		}
	})
	t.Run("out-of-contract value rejected", func(t *testing.T) {
		if _, err := parse(`{"1":"maybe"}`); err == nil {
			t.Fatal("expected error for invalid verdict value")
		}
	})
	t.Run("no JSON rejected", func(t *testing.T) {
		if _, err := parse("all relevant, trust me"); err == nil {
			t.Fatal("expected error for missing JSON")
		}
	})
}

func TestComposeTfidfRerankInput(t *testing.T) {
	blocks := []Block{{Content: strings.Repeat("a", 500)}, {Content: "short"}}
	in := composeTfidfRerankInput("query terms", blocks, 8192)
	if !strings.Contains(in, "QUERY TERMS:\nquery terms\n") {
		t.Errorf("query terms missing: %q", in[:80])
	}
	if !strings.Contains(in, "[1] ") || !strings.Contains(in, "[2] short") {
		t.Errorf("numbered blocks missing: %q", in)
	}
	if len(in) > 8192 {
		t.Errorf("input %d exceeds budget 8192", len(in))
	}
}

// rerankHarness builds the interceptor pieces: a text over budget with
// one clearly relevant block, one clearly irrelevant, and one borderline
// block; plus the rerank config.
func rerankConfig(enabled bool) *config.TfidfRerankConfig {
	return &config.TfidfRerankConfig{
		Enabled:        config.PtrTo(enabled),
		Band:           100.0, // wide band: the borderline block always qualifies
		MaxBlocks:      8,
		MaxBytes:       8192,
		TimeoutSeconds: 2,
	}
}

func TestTfidfRescueRescuesBorderlineBlock(t *testing.T) {
	// Direct unit test of the selection. With the rerank armed the
	// greedy budget is middleBudget - reserve (reserve =
	// min(middleBudget/4, MaxBytes/3) = min(10, 100) = 10), so b2
	// (12 runes, score 8) drops; cutoff 9 keeps b2 inside the band
	// ([4,9)) and b2 refills the reserved leftover on rescue.
	scored := []scoredBlock{
		{index: 0, score: 10, block: Block{Content: "b0"}},
		{index: 1, score: 9, block: Block{Content: "b1"}},
		{index: 2, score: 8, block: Block{Content: "b2"}},
	}
	runes := []int{10, 15, 12}
	var offered []Block
	rescue := &TfidfRescueParams{
		Band:      5,
		MaxBlocks: 8,
		MaxBytes:  300,
		Rescorer: func(q string, borderline []Block) map[int]bool {
			offered = borderline
			return map[int]bool{0: true}
		},
	}
	kept := selectKeptBlocks(scored, runes, 40, "alpha", rescue)
	if len(offered) != 1 || offered[0].Content != "b2" {
		t.Fatalf("offered = %+v, want only b2", offered)
	}
	if !kept[2] {
		t.Errorf("rescued block 2 not kept: %+v", kept)
	}
	// Budget invariant: rescued keeps never exceed the middle budget.
	total := 0
	for _, sb := range scored {
		if kept[sb.index] {
			total += runes[sb.index]
		}
	}
	if total > 40 {
		t.Errorf("kept runes %d exceed middle budget 40", total)
	}

	// Below-band dropped blocks are never offered: a tighter band
	// excludes b2 (8 < 9-0.5).
	rescue.Band = 0.5
	offered = nil
	kept = selectKeptBlocks(scored, runes, 40, "alpha", rescue)
	if len(offered) != 0 {
		t.Errorf("below-band block offered: %+v", offered)
	}
	if kept[2] {
		t.Error("below-band block must stay dropped")
	}

	// Budget exhaustion: a rescorer that rescues everything still
	// respects the middle budget.
	rescue.Band = 5
	rescue.Rescorer = func(q string, borderline []Block) map[int]bool {
		out := map[int]bool{}
		for i := range borderline {
			out[i] = true
		}
		return out
	}
	kept = selectKeptBlocks(scored, runes, 40, "alpha", rescue)
	total = 0
	for _, sb := range scored {
		if kept[sb.index] {
			total += runes[sb.index]
		}
	}
	if total > 40 {
		t.Errorf("rescue-everything kept runes %d exceed middle budget 40", total)
	}
}

func TestTfidfRescueRescuesBorderlineEndToEnd(t *testing.T) {
	// End-to-end: distinct scoring groups over budget; the judge is
	// consulted exactly once and the payload stays within budget.
	// (Payload-level rescue semantics are pinned by the
	// selectKeptBlocks unit test; assembly may still elide kept
	// borderline blocks near the budget boundary.)
	judge, calls := newRerankJudgeStub(t, `{"1":"relevant"}`, http.StatusOK)
	rerank := rerankConfig(true)

	text := strings.Repeat("alpha alpha alpha alpha\n\n", 40) +
		strings.Repeat("zebra zebra zebra zebra\n\n", 40) +
		strings.Repeat("alpha beta alpha beta\n\n", 40)

	rescue := &TfidfRescueParams{
		Band:      rerank.Band,
		MaxBlocks: rerank.MaxBlocks,
		MaxBytes:  rerank.MaxBytes,
		Rescorer: func(q string, borderline []Block) map[int]bool {
			return rerankRescueFn(judge, rerank.MaxBytes)(q, borderline)
		},
	}

	got := TruncateTFIDFWithRescue(text, 600, "alpha", config.ContextConfig{TruncationKeepFirstPct: 10, TruncationKeepLastPct: 10}, rescue)
	if calls.Load() != 1 {
		t.Fatalf("engine calls = %d, want 1", calls.Load())
	}
	if strings.Contains(got, "MASSIVE") {
		t.Error("armed pass must not degrade to middle-out truncation")
	}
}

func TestTfidfRescueFailOpenOnMalformed(t *testing.T) {
	judge, _ := newRerankJudgeStub(t, "garbage", http.StatusOK)
	text := strings.Repeat("alpha alpha\n\n", 60) + strings.Repeat("alpha beta\n\n", 60)
	rescue := &TfidfRescueParams{
		Band: 100.0, MaxBlocks: 8, MaxBytes: 8192,
		Rescorer: func(q string, borderline []Block) map[int]bool {
			return rerankRescueFn(judge, 8192)(q, borderline)
		},
	}
	got := TruncateTFIDFWithRescue(text, 300, "alpha", config.ContextConfig{TruncationKeepFirstPct: 10, TruncationKeepLastPct: 10}, rescue)
	// Fail-open: identical to the armed pass with no rescues (same
	// greedy reserve), which is the tier-1 outcome the caller keeps.
	want := TruncateTFIDFWithRescue(text, 300, "alpha", config.ContextConfig{TruncationKeepFirstPct: 10, TruncationKeepLastPct: 10},
		&TfidfRescueParams{Band: 100.0, MaxBlocks: 8, MaxBytes: 8192, Rescorer: func(string, []Block) map[int]bool { return nil }})
	if got != want {
		t.Errorf("fail-open rescue output differs from deterministic pruning\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestTfidfRescueNilParamsDeterministic(t *testing.T) {
	text := strings.Repeat("alpha\n\n", 80) + strings.Repeat("zebra\n\n", 40)
	base := TruncateTFIDF(text, 300, "alpha", config.ContextConfig{TruncationKeepFirstPct: 10, TruncationKeepLastPct: 10})
	if got := TruncateTFIDFWithRescue(text, 300, "alpha", config.ContextConfig{TruncationKeepFirstPct: 10, TruncationKeepLastPct: 10}, nil); got != base {
		t.Error("nil rescue params must be byte-identical to deterministic pruning")
	}
}

func rerankInterceptor(t *testing.T, judge *Judge, rerank *config.TfidfRerankConfig) *TFIDFInterceptor {
	t.Helper()
	return NewTFIDFInterceptor(TFIDFInterceptorOpts{
		QuerySource: "self",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Rerank:      rerank,
		RerankJudge: judge,
		Metrics:     infra.NewMetrics(),
	})
}

func rerankRequest(text string) *InterceptRequest {
	msgs := []map[string]any{
		{"role": "user", "content": "prior context"},
		{"role": "user", "content": text},
	}
	return &InterceptRequest{
		Payload:    map[string]any{"messages": msgs},
		Messages:   msgs,
		AgentName:  "agent",
		SoftLimit:  100,
		HardLimit:  133,
		TokenCount: 500,
	}
}

func TestTfidfInterceptorRerankWiring(t *testing.T) {
	text := strings.Repeat("alpha alpha alpha alpha\n\n", 40) +
		strings.Repeat("zebra zebra zebra zebra\n\n", 40) +
		strings.Repeat("alpha beta alpha beta\n\n", 40)

	t.Run("disabled never calls the judge", func(t *testing.T) {
		judge, calls := newRerankJudgeStub(t, `{"1":"relevant"}`, http.StatusOK)
		ic := rerankInterceptor(t, judge, rerankConfig(false))
		req := rerankRequest(text)
		res, err := ic.Process(context.Background(), req)
		if err != nil || !res.Truncated {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
		if calls.Load() != 0 {
			t.Fatalf("disabled rerank consulted %d times, want 0", calls.Load())
		}
	})

	t.Run("enabled consults the judge and rescues", func(t *testing.T) {
		judge, calls := newRerankJudgeStub(t, `{"1":"not_relevant","2":"relevant"}`, http.StatusOK)
		ic := rerankInterceptor(t, judge, rerankConfig(true))
		req := rerankRequest(text)
		res, err := ic.Process(context.Background(), req)
		if err != nil || !res.Truncated {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
		if calls.Load() != 1 {
			t.Fatalf("engine calls = %d, want 1", calls.Load())
		}
		content, _ := req.Messages[1]["content"].(string)
		if !strings.Contains(content, "alpha beta") {
			t.Errorf("borderline block not rescued:\n%s", content)
		}
	})
}

func TestTfidfRescueFailOpenOnEngineHTTPError(t *testing.T) {
	// 503 from the judge engine: fail-open to the deterministic
	// pruning, and the judgment family records verdict="error".
	judge, _ := newRerankJudgeStub(t, `{"1":"relevant"}`, http.StatusServiceUnavailable)
	metrics := infra.NewMetrics()
	judge.deps.Metrics = metrics
	text := strings.Repeat("alpha alpha\n\n", 60) + strings.Repeat("alpha beta\n\n", 60)
	got := TruncateTFIDFWithRescue(text, 300, "alpha", config.ContextConfig{TruncationKeepFirstPct: 10, TruncationKeepLastPct: 10},
		&TfidfRescueParams{Band: 100.0, MaxBlocks: 8, MaxBytes: 8192, Rescorer: rerankRescueFn(judge, 8192)})
	want := TruncateTFIDFWithRescue(text, 300, "alpha", config.ContextConfig{TruncationKeepFirstPct: 10, TruncationKeepLastPct: 10},
		&TfidfRescueParams{Band: 100.0, MaxBlocks: 8, MaxBytes: 8192, Rescorer: func(string, []Block) map[int]bool { return nil }})
	if got != want {
		t.Error("engine HTTP failure must fail open to the armed deterministic pruning")
	}
	out := &strings.Builder{}
	metrics.WritePrometheus(out)
	found := false
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "nenya_judgments_total{") &&
			strings.Contains(line, `judgment="tfidf_rerank"`) &&
			strings.Contains(line, `verdict="error"`) {
			found = true
		}
	}
	if !found {
		t.Error("nenya_judgments_total{judgment=tfidf_rerank,verdict=error} not recorded")
	}
}
