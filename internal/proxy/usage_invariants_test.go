package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/testutil"
)

// usageFixtureGW builds the minimum gateway for exercising the usage recording
// path with real pricing and cost accounting.
func usageFixtureGW() *gateway.NenyaGateway {
	catalog := discovery.NewModelCatalog()
	catalog.Add(discovery.DiscoveredModel{ID: "fixture-model", Provider: "fixture", Pricing: &discovery.PricingEntry{
		InputCostPer1M:       1.0,
		OutputCostPer1M:      2.0,
		CachedInputCostPer1M: 0.5,
	}})
	return &gateway.NenyaGateway{
		Providers:    map[string]*config.Provider{"fixture": {Name: "fixture"}},
		ModelCatalog: catalog,
		CostTracker:  infra.NewCostTracker(),
		Stats:        infra.NewUsageTracker(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func fixtureTarget() routing.UpstreamTarget {
	return routing.UpstreamTarget{Provider: "fixture", Model: "fixture-model"}
}

// fixtureStats returns the recorded per-model counters.
func fixtureStats(t *testing.T, gw *gateway.NenyaGateway) map[string]interface{} {
	t.Helper()
	snapshot := gw.Stats.Snapshot()
	models, ok := snapshot["models"].(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected stats snapshot shape: %v", snapshot)
	}
	model, ok := models["fixture-model"].(map[string]interface{})
	if !ok {
		t.Fatalf("no stats recorded for fixture-model: %v", models)
	}
	return model
}

func statValue(t *testing.T, model map[string]interface{}, key string) int {
	t.Helper()
	switch v := model[key].(type) {
	case uint64:
		return int(v)
	case int:
		return v
	default:
		t.Fatalf("stat %q has unexpected type %T (%v)", key, model[key], model[key])
		return 0
	}
}

// TestUsageRecordedForEveryProviderShape is the per-provider extraction
// verification pass (NENYA-167): every usage shape Nenya accepts must produce
// non-zero output accounting and a non-zero cost, with each dimension landing
// in exactly one bucket.
func TestUsageRecordedForEveryProviderShape(t *testing.T) {
	tests := []struct {
		name          string
		usage         map[string]interface{}
		wantOutput    int
		wantCacheHit  int
		wantCreation  int
		wantReasoning int
	}{
		{
			name:       "openai standard",
			usage:      map[string]interface{}{"prompt_tokens": 100.0, "completion_tokens": 20.0, "total_tokens": 120.0},
			wantOutput: 20,
		},
		{
			name: "openai nested cached details",
			usage: map[string]interface{}{
				"prompt_tokens": 100.0, "completion_tokens": 20.0, "total_tokens": 120.0,
				"prompt_tokens_details": map[string]interface{}{"cached_tokens": 60.0},
			},
			wantOutput:   20,
			wantCacheHit: 60,
		},
		{
			name: "openai reasoning details",
			usage: map[string]interface{}{
				"prompt_tokens": 10.0, "completion_tokens": 20.0, "total_tokens": 30.0,
				"completion_tokens_details": map[string]interface{}{"reasoning_tokens": 12.0},
			},
			wantOutput:    20,
			wantReasoning: 12,
		},
		{
			name: "anthropic native",
			usage: map[string]interface{}{
				"input_tokens": 30.0, "output_tokens": 10.0,
				"cache_read_input_tokens": 60.0, "cache_creation_input_tokens": 10.0,
			},
			wantOutput:   10,
			wantCacheHit: 60,
			wantCreation: 10,
		},
		{
			name: "anthropic native thinking",
			usage: map[string]interface{}{
				"input_tokens": 30.0, "output_tokens": 10.0, "completion_reasoning_tokens": 4.0,
			},
			wantOutput:    10,
			wantReasoning: 4,
		},
		{
			name: "gemini native metadata",
			usage: map[string]interface{}{
				"promptTokenCount": 25.0, "candidatesTokenCount": 40.0, "thoughtsTokenCount": 15.0,
				"totalTokenCount": 80.0, "cachedContentTokenCount": 5.0,
			},
			wantOutput:    55,
			wantCacheHit:  5,
			wantReasoning: 15,
		},
		{
			// Shape-identical to the OpenAI case on purpose: this is the
			// usage-only final chunk (no choices) that Together-class providers
			// send. The placement itself is covered by the reader test
			// TestReaderUsageOnlyFinalChunk.
			name: "together-style usage-only final chunk",
			usage: map[string]interface{}{
				"prompt_tokens": 11.0, "completion_tokens": 7.0, "total_tokens": 18.0,
			},
			wantOutput: 7,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gw := usageFixtureGW()
			recordNonStreamingUsage(context.Background(), gw, fixtureTarget(), "agent", "", tc.usage)

			model := fixtureStats(t, gw)
			if got := statValue(t, model, "output_tokens"); got != tc.wantOutput {
				t.Errorf("output_tokens = %d, want %d", got, tc.wantOutput)
			}
			if got := statValue(t, model, "cache_hit_tokens"); got != tc.wantCacheHit {
				t.Errorf("cache_hit_tokens = %d, want %d", got, tc.wantCacheHit)
			}
			if got := statValue(t, model, "cache_creation_tokens"); got != tc.wantCreation {
				t.Errorf("cache_creation_tokens = %d, want %d", got, tc.wantCreation)
			}
			if got := statValue(t, model, "reasoning_tokens"); got != tc.wantReasoning {
				t.Errorf("reasoning_tokens = %d, want %d", got, tc.wantReasoning)
			}
			if cost := gw.CostTracker.GetCostMicroUSD("fixture-model"); cost <= 0 {
				t.Errorf("cost = %d microUSD, want non-zero (usage must reach the cost model)", cost)
			}
		})
	}
}

// TestUsageCacheDimensionsCountedOnceInCost pins invariant I1 (NENYA-167): a
// cache-write token must not be billed as ordinary input on top of the prompt
// total that already includes it, and the native cache-read field must not be
// added twice when an OpenAI-style decomposition is present.
func TestUsageCacheDimensionsCountedOnceInCost(t *testing.T) {
	baseline := usageFixtureGW()
	recordNonStreamingUsage(context.Background(), baseline, fixtureTarget(), "agent", "",
		map[string]interface{}{"prompt_tokens": 100.0, "completion_tokens": 20.0})
	baseCost := baseline.CostTracker.GetCostMicroUSD("fixture-model")

	t.Run("cache-write is not extra input", func(t *testing.T) {
		gw := usageFixtureGW()
		recordNonStreamingUsage(context.Background(), gw, fixtureTarget(), "agent", "",
			map[string]interface{}{
				"prompt_tokens": 100.0, "completion_tokens": 20.0,
				"cache_creation_tokens": 10.0, "prompt_cache_hit_tokens": 60.0,
			})
		// 60 cached @ 0.5/1M = 30 microUSD + 40 remaining input = 40 + 20 output
		// @ 2.0/1M = 40 → 110. A prompt total inflated by the cache-write
		// dimension (110 input) would price 120.
		if got := gw.CostTracker.GetCostMicroUSD("fixture-model"); got != 110 {
			t.Errorf("cost = %d microUSD, want exactly 110", got)
		}
		if baseCost != 140 {
			t.Errorf("baseline cost = %d microUSD, want 140", baseCost)
		}
	})

	t.Run("ambiguous native cache semantics fold once", func(t *testing.T) {
		// An input total under the OpenAI key name with the native cache field
		// names and no decomposition is read as Anthropic input semantics: the
		// cache dimensions are folded into the total. The cost must match the
		// equivalent explicit OpenAI total (input 160 = 90 + 60 read + 10
		// written, 60 of it cached).
		ambiguous := usageFixtureGW()
		recordNonStreamingUsage(context.Background(), ambiguous, fixtureTarget(), "agent", "",
			map[string]interface{}{
				"prompt_tokens": 90.0, "completion_tokens": 20.0,
				"cache_read_input_tokens": 60.0, "cache_creation_input_tokens": 10.0,
			})
		explicit := usageFixtureGW()
		recordNonStreamingUsage(context.Background(), explicit, fixtureTarget(), "agent", "",
			map[string]interface{}{
				"prompt_tokens": 160.0, "completion_tokens": 20.0,
				"prompt_cache_hit_tokens": 60.0, "cache_creation_tokens": 10.0,
			})
		if a, b := ambiguous.CostTracker.GetCostMicroUSD("fixture-model"), explicit.CostTracker.GetCostMicroUSD("fixture-model"); a != b {
			t.Errorf("ambiguous cost = %d, explicit cost = %d; the fold must count each dimension once", a, b)
		}
		if ambiguous.CostTracker.GetCostMicroUSD("fixture-model") <= baseCost {
			t.Errorf("folded cost = %d, want more than the baseline %d (the input total includes the cache dimensions)",
				ambiguous.CostTracker.GetCostMicroUSD("fixture-model"), baseCost)
		}
	})

	t.Run("native cache read is not added twice", func(t *testing.T) {
		// Anthropic-native input_tokens EXCLUDES the cache dimensions, so the
		// folded total must still price the same input volume as an explicit
		// OpenAI total of input+read+write.
		native := usageFixtureGW()
		recordNonStreamingUsage(context.Background(), native, fixtureTarget(), "agent", "",
			map[string]interface{}{
				"input_tokens": 30.0, "output_tokens": 20.0,
				"cache_read_input_tokens": 60.0, "cache_creation_input_tokens": 10.0,
			})
		openaiShape := usageFixtureGW()
		recordNonStreamingUsage(context.Background(), openaiShape, fixtureTarget(), "agent", "",
			map[string]interface{}{
				"prompt_tokens": 100.0, "completion_tokens": 20.0,
				"prompt_cache_hit_tokens": 60.0, "cache_creation_tokens": 10.0,
			})
		if a, b := native.CostTracker.GetCostMicroUSD("fixture-model"), openaiShape.CostTracker.GetCostMicroUSD("fixture-model"); a != b {
			t.Errorf("native cost = %d, openai-shaped cost = %d; the same token volume must price identically", a, b)
		}
	})
}

// TestReasoningTokensAreInsideOutputTotal pins invariant I2 (NENYA-167):
// reasoning/thinking tokens are a subset of the output total, so they never add
// to the billed output. For Gemini's native shape the thought tokens are folded
// INTO the completion total, so the cost matches the equivalent OpenAI shape.
func TestReasoningTokensAreInsideOutputTotal(t *testing.T) {
	withReasoning := usageFixtureGW()
	recordNonStreamingUsage(context.Background(), withReasoning, fixtureTarget(), "agent", "",
		map[string]interface{}{
			"prompt_tokens": 10.0, "completion_tokens": 20.0,
			"completion_tokens_details": map[string]interface{}{"reasoning_tokens": 12.0},
		})
	withoutReasoning := usageFixtureGW()
	recordNonStreamingUsage(context.Background(), withoutReasoning, fixtureTarget(), "agent", "",
		map[string]interface{}{"prompt_tokens": 10.0, "completion_tokens": 20.0})
	if a, b := withReasoning.CostTracker.GetCostMicroUSD("fixture-model"), withoutReasoning.CostTracker.GetCostMicroUSD("fixture-model"); a != b {
		t.Errorf("reasoning changed the cost (%d vs %d): it must be a subset of the output total", a, b)
	}

	// Gemini native: candidates exclude the thoughts, so 40 + 15 must price
	// like an OpenAI completion total of 55.
	gemini := usageFixtureGW()
	recordNonStreamingUsage(context.Background(), gemini, fixtureTarget(), "agent", "",
		map[string]interface{}{
			"promptTokenCount": 25.0, "candidatesTokenCount": 40.0, "thoughtsTokenCount": 15.0,
		})
	equivalent := usageFixtureGW()
	recordNonStreamingUsage(context.Background(), equivalent, fixtureTarget(), "agent", "",
		map[string]interface{}{"prompt_tokens": 25.0, "completion_tokens": 55.0})
	if a, b := gemini.CostTracker.GetCostMicroUSD("fixture-model"), equivalent.CostTracker.GetCostMicroUSD("fixture-model"); a != b {
		t.Errorf("gemini cost = %d, equivalent openai cost = %d; thought tokens must be inside the completion total", a, b)
	}
}

// TestUsageRecordedWhenEgressBlocksTheResponse pins invariant I3 on the
// termination path: a response that a deterministic egress defense blocks (the
// upstream already billed it) must still have its usage recorded, exactly once.
// This is the coverage the removed recordAndBlockBuffered call sites used to
// provide.
func TestUsageRecordedWhenEgressBlocksTheResponse(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"see http://169.254.169.254/latest/meta-data/ end"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`))
	}))
	defer up.Close()

	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(600)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(10000000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Governance.ExfilGuard = &config.ExfilGuardConfig{
		Enabled: config.PtrTo(true),
		Action:  config.ExfilActionBlock,
	}
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider-0": {URL: up.URL + "/v1/chat/completions", AuthStyle: "none"},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models:   []config.AgentModel{{Provider: "test-provider-0", Model: "test-model"}},
		},
	}
	secrets := &config.SecretsConfig{ClientToken: "test-token"}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p := &Proxy{}
	p.StoreGateway(gw)

	body := `{"model":"test-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("expected the egress guard to block the response, got 200: %s", rec.Body.String())
	}
	snapshot := gw.Stats.Snapshot()
	models, _ := snapshot["models"].(map[string]interface{})
	model, ok := models["test-model"].(map[string]interface{})
	if !ok {
		t.Fatalf("blocked response recorded no usage: %v", models)
	}
	if got, _ := model["output_tokens"].(uint64); got != 20 {
		t.Errorf("output_tokens = %d, want 20 (the upstream billed the blocked reply)", got)
	}
}

// TestNonStreamingUsageRecordedBeforeFormatConversion pins invariant I3
// (NENYA-167) end to end: an Anthropic-format client talking to an
// OpenAI-format upstream gets its usage recorded even though the response is
// converted (and an egress policy could terminate it) after the upstream
// reported it. The conversion renames prompt_tokens/completion_tokens to
// input_tokens/output_tokens, so recording after it used to lose the usage
// entirely.
func TestNonStreamingUsageRecordedBeforeFormatConversion(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":60}}}`))
	}))
	defer up.Close()

	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(600)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(10000000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider-0": {URL: up.URL + "/v1/chat/completions", AuthStyle: "none"},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models:   []config.AgentModel{{Provider: "test-provider-0", Model: "test-model"}},
		},
	}
	secrets := &config.SecretsConfig{ClientToken: "test-token"}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p := &Proxy{}
	p.StoreGateway(gw)

	// Anthropic wire format (the client's format), non-streaming.
	body := `{"type":"message","model":"test-agent","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The client sees the Anthropic shape.
	if !strings.Contains(rec.Body.String(), `"input_tokens"`) {
		t.Errorf("expected an Anthropic-shaped response, got %s", rec.Body.String())
	}

	snapshot := gw.Stats.Snapshot()
	models, _ := snapshot["models"].(map[string]interface{})
	model, ok := models["test-model"].(map[string]interface{})
	if !ok {
		t.Fatalf("no usage recorded for test-model: %v", models)
	}
	if got, _ := model["output_tokens"].(uint64); got != 20 {
		t.Errorf("output_tokens = %d, want 20 (usage must survive the format conversion)", got)
	}
	if got, _ := model["cache_hit_tokens"].(uint64); got != 60 {
		t.Errorf("cache_hit_tokens = %d, want 60", got)
	}
}

// TestMCPBufferUsageMergesPlacements pins the MCP buffered aggregation
// (NENYA-167): an Anthropic-native buffer reports the input side on
// message_start and the output side on message_delta, so the merged totals (and
// the calibration actual) must carry both instead of only the last chunk.
func TestMCPBufferUsageMergesPlacements(t *testing.T) {
	raw := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":30,"cache_read_input_tokens":60,"cache_creation_input_tokens":10}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":12}}`,
		``,
	}, "\n")
	buf := &bufferedSSE{rawBytes: []byte(raw), model: "fixture-model"}

	totals, ok := buf.usageTotals()
	if !ok {
		t.Fatal("expected the buffer's usage to be found")
	}
	if totals.Prompt != 100 {
		t.Errorf("prompt = %d, want 100", totals.Prompt)
	}
	if totals.Completion != 12 {
		t.Errorf("completion = %d, want 12", totals.Completion)
	}
	if totals.Total != 112 {
		t.Errorf("total = %d, want 112", totals.Total)
	}
	if totals.CacheHit != 60 || totals.CacheCreation != 10 || totals.CacheMiss != 30 {
		t.Errorf("cache counters = %d/%d/%d, want hit 60 / miss 30 / creation 10", totals.CacheHit, totals.CacheMiss, totals.CacheCreation)
	}

	// The OpenAI shape (one cumulative object) merges to the same values.
	openAI := &bufferedSSE{rawBytes: []byte(`data: {"id":"c1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":12,"total_tokens":112,"prompt_cache_hit_tokens":60,"cache_creation_tokens":10}}`), model: "fixture-model"}
	merged, ok := openAI.usageTotals()
	if !ok || merged.Prompt != 100 || merged.Completion != 12 || merged.Total != 112 {
		t.Fatalf("openai buffer totals = %+v (ok=%v)", merged, ok)
	}
}
