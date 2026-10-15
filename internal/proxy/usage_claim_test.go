package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/billing"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
	"github.com/nenya/internal/testutil"
)

// claimFixtureGW builds a gateway with pricing, cost, billing and stats so the
// single-claim semantics can be asserted end to end.
func claimFixtureGW() *gateway.NenyaGateway {
	catalog := discovery.NewModelCatalog()
	catalog.Add(discovery.DiscoveredModel{ID: "fixture-model", Provider: "fixture", Pricing: &discovery.PricingEntry{
		InputCostPer1M:  1.0,
		OutputCostPer1M: 2.0,
	}})
	return &gateway.NenyaGateway{
		Providers:      map[string]*config.Provider{"fixture": {Name: "fixture"}},
		ModelCatalog:   catalog,
		CostTracker:    infra.NewCostTracker(),
		Stats:          infra.NewUsageTracker(),
		BillingTracker: billing.NewBillingTracker(nil, nil),
		Metrics:        infra.NewMetrics(),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestStreamingUsageClaimsOneBillingRequest pins NENYA-193: a provider that
// reports usage in several cumulative chunks produces exactly one billing
// request (and one spend event) for the dispatch, with the cost and token
// counters equal to the request's cumulative values — not one per chunk.
func TestStreamingUsageClaimsOneBillingRequest(t *testing.T) {
	gw := claimFixtureGW()
	cb := (&Proxy{}).makeUsageCallback(context.Background(), gw, routing.UpstreamTarget{Provider: "fixture", Model: "fixture-model"}, "agent")

	// The wire values are cumulative (10/5, then 20/12, then 20/20); the
	// callback receives the deltas (10/5, 10/7, 0/8).
	cb(stream.UsageData{PromptTokens: 1_000_000, CompletionTokens: 500_000})
	cb(stream.UsageData{PromptTokens: 1_000_000, CompletionTokens: 700_000})
	cb(stream.UsageData{PromptTokens: 0, CompletionTokens: 800_000})

	if got := gw.BillingTracker.TotalRequests.Load(); got != 1 {
		t.Errorf("billing requests = %d, want 1 (one dispatch)", got)
	}
	// 2M input @ 1.0/1M + 2M output @ 2.0/1M = 2.00 + 4.00 = 6.00 USD.
	if got := gw.CostTracker.GetCostMicroUSD("fixture-model"); got != 6_000_000 {
		t.Errorf("cost = %d microUSD, want 6000000", got)
	}
	// The spend side is exact too: the first delta claims the request with its
	// cost, the later deltas only add.
	if got := gw.BillingTracker.TotalSpendCents.Load(); got != 600 {
		t.Errorf("spend cents = %d, want 600 (2.00 + 4.00 USD)", got)
	}

	models, _ := gw.Stats.Snapshot()["models"].(map[string]interface{})
	model, ok := models["fixture-model"].(map[string]interface{})
	if !ok {
		t.Fatalf("no stats recorded: %v", models)
	}
	if got, _ := model["output_tokens"].(uint64); got != 2_000_000 {
		t.Errorf("output_tokens = %d, want the cumulative 2000000 (each delta counted once)", got)
	}

	// A fresh per-attempt callback (a retry/failover attempt) claims its own
	// request: the claim is per dispatch, not per client request.
	cb2 := (&Proxy{}).makeUsageCallback(context.Background(), gw, routing.UpstreamTarget{Provider: "fixture", Model: "fixture-model"}, "agent")
	cb2(stream.UsageData{PromptTokens: 5, CompletionTokens: 5})
	if got := gw.BillingTracker.TotalRequests.Load(); got != 2 {
		t.Errorf("billing requests after a second dispatch = %d, want 2", got)
	}
}

// TestRetriedRequestClaimsOncePerDispatch pins NENYA-193 on the retry path: a
// request whose first attempt fails (no usage) and whose second attempt
// succeeds produces exactly one client-visible usage record and one billed
// dispatch.
func TestRetriedRequestClaimsOncePerDispatch(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"transient"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`))
	}))
	defer upstream.Close()

	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(600)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(10000000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Governance.MaxRetryAttempts = 2
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider-0": {URL: upstream.URL + "/v1/chat/completions", AuthStyle: "none"},
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

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after the retry, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (failed attempt + retry)", got)
	}

	// The failed attempt billed nothing; the successful dispatch is recorded
	// exactly once (one client-visible usage object).
	models, _ := gw.Stats.Snapshot()["models"].(map[string]interface{})
	model, ok := models["test-model"].(map[string]interface{})
	if !ok {
		t.Fatalf("no usage recorded for the successful attempt: %v", models)
	}
	if got, _ := model["output_tokens"].(uint64); got != 20 {
		t.Errorf("output_tokens = %d, want 20 (recorded once, not once per attempt)", got)
	}
	// The failed attempt billed nothing; the successful dispatch claims one
	// request.
	if got := gw.BillingTracker.TotalRequests.Load(); got != 1 {
		t.Errorf("billing requests = %d, want 1 (the failed attempt claimed nothing)", got)
	}
}

// TestMCPUsageClaimedOncePerBuffer pins NENYA-193's claim flag: a buffer whose
// usage was already claimed (by the per-iteration claim, or by an earlier
// terminal path) is not recorded again, and a buffer with no usable usage does
// not consume the claim (so a later valid read still records).
func TestMCPUsageClaimedOncePerBuffer(t *testing.T) {
	p := &Proxy{}
	gw := usageFixtureGW()
	buf := &bufferedSSE{
		rawBytes: []byte(`data: {"id":"c1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`),
		model:    "fixture-model",
	}

	p.recordMCPUsage(gw, buf, "agent")
	p.recordMCPUsage(gw, buf, "agent")

	model := fixtureStats(t, gw)
	if got := statValue(t, model, "output_tokens"); got != 20 {
		t.Errorf("output_tokens = %d, want 20 (claimed once per buffer)", got)
	}

	// A buffer with no usable usage must not consume the claim, so a terminal
	// path observing it does not suppress a later legitimate record for the
	// same buffer (the flag is set only when a record is actually written).
	empty := &bufferedSSE{rawBytes: []byte("data: {\"choices\":[]}"), model: "fixture-model"}
	p.recordMCPUsage(gw, empty, "agent")
	if empty.usageClaimed {
		t.Error("a buffer with no usage must not consume the claim")
	}
	if got := statValue(t, fixtureStats(t, gw), "output_tokens"); got != 20 {
		t.Errorf("output_tokens = %d, want 20 (nothing recorded for the usage-less buffer)", got)
	}
}

// TestContinuedStreamClaimsPerDispatch pins NENYA-193's attribution rule on the
// continuation path: a cut stream continued on the same target is two billable
// upstream dispatches, so each claims its own record (Stats output sums both),
// while each dispatch's own usage is counted once (no prefix re-count).
func TestContinuedStreamClaimsPerDispatch(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			// First dispatch: one content chunk carrying usage, then a cut.
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
			return
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":7,\"total_tokens\":17}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(600)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(10000000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Governance.StreamContinuation = &config.StreamContinuationConfig{
		Enabled:       config.PtrTo(true),
		MaxAttempts:   2,
		SameModelOnly: config.PtrTo(true),
	}
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider-0": {URL: upstream.URL + "/v1/chat/completions", AuthStyle: "none"},
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

	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (original + continuation)", got)
	}

	models, _ := gw.Stats.Snapshot()["models"].(map[string]interface{})
	model, ok := models["test-model"].(map[string]interface{})
	if !ok {
		t.Fatalf("no usage recorded for the continued stream: %v", models)
	}
	// Each dispatch recorded its own output once: 5 (first) + 7 (continuation).
	if got, _ := model["output_tokens"].(uint64); got != 12 {
		t.Errorf("output_tokens = %d, want 12 (5 + 7, one record per dispatch)", got)
	}
	// Two dispatches reached the provider, so two billing requests were
	// claimed (the continuation is a separate billable upstream call).
	if got := gw.BillingTracker.TotalRequests.Load(); got != 2 {
		t.Errorf("billing requests = %d, want 2 (one per dispatch)", got)
	}
	respBody := rec.Body.String()
	if !strings.Contains(respBody, "hello ") || !strings.Contains(respBody, "world") {
		t.Errorf("expected the concatenated continuation stream, got %s", respBody)
	}
}

// TestMCPIterationUsageClaimsOnce pins the MCP per-dispatch claim (NENYA-193):
// one loop iteration is one upstream dispatch, so it claims Stats (every
// dimension), cost and exactly one billing request, attributed to the target
// the buffer was dispatched to. A second claim on the same buffer is a no-op.
func TestMCPIterationUsageClaimsOnce(t *testing.T) {
	gw := usageFixtureGW()
	gw.BillingTracker = billing.NewBillingTracker(nil, nil)
	gw.Metrics = infra.NewMetrics()
	p := &Proxy{}

	// Two targets offer the same model under different providers: the recorded
	// serving target decides the attribution.
	targets := []routing.UpstreamTarget{
		{Provider: "fixture", Model: "fixture-model", AccountName: "acct-a"},
		{Provider: "fixture", Model: "fixture-model", AccountName: "acct-b"},
	}
	served := targets[1]
	buf := &bufferedSSE{
		rawBytes:      []byte(`data: {"id":"c1","choices":[],"usage":{"prompt_tokens":1000000,"completion_tokens":1000000,"total_tokens":2000000,"prompt_cache_hit_tokens":600000,"cache_creation_tokens":100000}}`),
		model:         "fixture-model",
		servingTarget: served,
	}

	p.recordMCPIterationUsage(context.Background(), gw, buf, targets, "agent")
	p.recordMCPIterationUsage(context.Background(), gw, buf, targets, "agent")

	model := fixtureStats(t, gw)
	if got := statValue(t, model, "output_tokens"); got != 1_000_000 {
		t.Errorf("output_tokens = %d, want 1000000", got)
	}
	if got := statValue(t, model, "cache_hit_tokens"); got != 600_000 {
		t.Errorf("cache_hit_tokens = %d, want 600000", got)
	}
	if got := statValue(t, model, "cache_creation_tokens"); got != 100_000 {
		t.Errorf("cache_creation_tokens = %d, want 100000", got)
	}
	if got := gw.BillingTracker.TotalRequests.Load(); got != 1 {
		t.Errorf("billing requests = %d, want 1 (one claim per dispatch)", got)
	}
	// 0.6M cached @ 0.5/1M + 0.4M remaining input @ 1.0/1M + 1M output @
	// 2.0/1M = 0.30 + 0.40 + 2.00 = 2.70 USD.
	if got := gw.CostTracker.GetCostMicroUSD("fixture-model"); got != 2_700_000 {
		t.Errorf("cost = %d microUSD, want 2700000", got)
	}
	// The spend is attributed to the served account, not the first target.
	if got := gw.BillingTracker.GetTotalSpend("fixture", "acct-b"); got <= 0 {
		t.Errorf("spend for acct-b = %v, want > 0 (the recorded serving target)", got)
	}
	if got := gw.BillingTracker.GetTotalSpend("fixture", "acct-a"); got != 0 {
		t.Errorf("spend for acct-a = %v, want 0 (not the served account)", got)
	}

	// A buffer without a recorded serving target falls back to the chain
	// resolution (the first target offering the model).
	fallback := &bufferedSSE{
		rawBytes: []byte(`data: {"choices":[],"usage":{"prompt_tokens":1000000,"completion_tokens":1000000,"total_tokens":2000000}}`),
		model:    "fixture-model",
	}
	p.recordMCPIterationUsage(context.Background(), gw, fallback, targets, "agent")
	if got := gw.BillingTracker.GetTotalSpend("fixture", "acct-a"); got <= 0 {
		t.Errorf("fallback spend for acct-a = %v, want > 0", got)
	}
}
