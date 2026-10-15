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
	"github.com/nenya/internal/auth"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/mcp"
	"github.com/nenya/internal/testutil"
)

// budgetChainProxy builds a proxy whose agent chain is the given providers (in
// order) and whose key carries the given daily budget/tier. The upstreams are
// supplied per provider so a test can assert which target served.
func budgetChainProxy(t *testing.T, providers []config.ProviderConfig, key config.ApiKey, maxRPM, maxTPM int) (*Proxy, *gateway.NenyaGateway) {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(maxRPM)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(maxTPM)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	// One same-target retry keeps the failover tests fast (the retry loop
	// sleeps between same-target attempts).
	cfg.Governance.MaxRetryAttempts = 1
	cfg.Providers = map[string]config.ProviderConfig{}
	models := make([]config.AgentModel, 0, len(providers))
	for i, p := range providers {
		name := fmt.Sprintf("p%d", i+1)
		cfg.Providers[name] = p
		models = append(models, config.AgentModel{Provider: name, Model: "test-model"})
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {Strategy: "fallback", Models: models},
	}
	key.Name = "budget-key"
	key.Token = "budget-key-token-1234567890"
	key.Roles = []string{"user"}
	key.Enabled = true
	secrets := &config.SecretsConfig{
		ClientToken: "primary-token-1234567890",
		ApiKeys:     map[string]config.ApiKey{"budget-key": key},
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p := &Proxy{}
	p.StoreGateway(gw)
	return p, gw
}

func okUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	t.Cleanup(up.Close)
	return up, &calls
}

// sseUpstream serves an SSE chat completion with no tool call, so the MCP loop
// runs exactly one iteration and then replays the buffered response.
func sseUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(up.Close)
	return up, &calls
}

func budgetRequest() *http.Request {
	// A long prompt so the token estimate comfortably exceeds a 1-token
	// provider budget (the budget guard compares the request estimate).
	body := `{"model":"test-agent","stream":false,"messages":[{"role":"user","content":"please tell me a fairly long story about dragons and castles and the many adventures of their keepers in the high mountains"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer budget-key-token-1234567890")
	return req
}

// TestProviderBudgetExhaustedTargetIsSkipped pins NENYA-188's failover rule: a
// target whose provider daily budget is exhausted (for a "fill"-tier key) is
// skipped so the sweep can serve from a later target with headroom, instead of
// failing the whole request.
func TestProviderBudgetExhaustedTargetIsSkipped(t *testing.T) {
	first, firstCalls := okUpstream(t)
	second, secondCalls := okUpstream(t)

	p, gw := budgetChainProxy(t, []config.ProviderConfig{
		{URL: first.URL, AuthStyle: "none", TokenBudgetDaily: 1},
		{URL: second.URL, AuthStyle: "none"},
	}, config.ApiKey{TokenBudgetDaily: 100, BudgetTier: "fill"}, 600, 10_000_000)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, budgetRequest())

	if rec.Code != http.StatusOK {
		t.Fatalf("expected the request to be served by the second target, got %d: %s", rec.Code, rec.Body.String())
	}
	if firstCalls.Load() != 0 {
		t.Errorf("budget-exhausted target was dispatched %d times, want 0", firstCalls.Load())
	}
	if secondCalls.Load() != 1 {
		t.Errorf("second target dispatched %d times, want 1", secondCalls.Load())
	}
	// The skipped provider must not have consumed any budget.
	providers, _ := gw.KeyUsage.Snapshot()["providers"].(map[string]interface{})
	if entry, ok := providers["p1"].(map[string]int); ok && entry["day_tokens"] != 0 {
		t.Errorf("skipped provider p1 day_tokens = %d, want 0", entry["day_tokens"])
	}
}

// TestProviderBudgetExhaustedEverywhereReportsBudget pins the terminal case: if
// every target's provider budget is exhausted, the client gets the budget
// reason (429 rate_limited with provider_budget) rather than a generic
// exhaustion.
func TestProviderBudgetExhaustedEverywhereReportsBudget(t *testing.T) {
	first, firstCalls := okUpstream(t)
	second, secondCalls := okUpstream(t)

	p, gw := budgetChainProxy(t, []config.ProviderConfig{
		{URL: first.URL, AuthStyle: "none", TokenBudgetDaily: 1},
		{URL: second.URL, AuthStyle: "none", TokenBudgetDaily: 1},
	}, config.ApiKey{TokenBudgetDaily: 100, BudgetTier: "fill"}, 600, 10_000_000)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, budgetRequest())

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "provider_budget") {
		t.Errorf("expected the provider_budget reason, got %s", rec.Body.String())
	}
	if firstCalls.Load()+secondCalls.Load() != 0 {
		t.Errorf("budget-exhausted targets were dispatched (%d + %d), want 0", firstCalls.Load(), secondCalls.Load())
	}
	var buf strings.Builder
	gw.Metrics.WritePrometheus(&buf)
	assertDenialMetric(t, buf.String(), "provider_budget")
}

// TestKeyBudgetChargedOnceAcrossFailover pins NENYA-188's single-reservation
// rule: a multi-target sweep charges the key's daily budget exactly once for the
// logical request, even though each target is budget-checked separately.
func TestKeyBudgetChargedOnceAcrossFailover(t *testing.T) {
	// The first target fails (5xx) so the sweep advances to the second.
	var firstCalls atomic.Int32
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	t.Cleanup(failing.Close)
	second, secondCalls := okUpstream(t)

	p, gw := budgetChainProxy(t, []config.ProviderConfig{
		{URL: failing.URL, AuthStyle: "none"},
		{URL: second.URL, AuthStyle: "none"},
	}, config.ApiKey{TokenBudgetDaily: 10_000_000}, 600, 10_000_000)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, budgetRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("expected the failover target to serve, got %d: %s", rec.Code, rec.Body.String())
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("dispatches = %d/%d, want 1/1", firstCalls.Load(), secondCalls.Load())
	}

	keys, ok := gw.KeyUsage.Snapshot()["keys"].(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected key usage snapshot: %v", gw.KeyUsage.Snapshot())
	}
	entry, ok := keys["budget-key"].(map[string]int)
	if !ok {
		t.Fatalf("no usage recorded for budget-key: %v", keys)
	}
	// The charge is the request's token estimate once, not once per dispatch.
	if entry["day_tokens"] <= 0 {
		t.Errorf("day_tokens = %d, want a single positive charge", entry["day_tokens"])
	}
	estimate := gw.CountRequestTokens(map[string]interface{}{
		"model": "test-agent",
		"messages": []interface{}{map[string]interface{}{
			"role":    "user",
			"content": "please tell me a fairly long story about dragons and castles and the many adventures of their keepers in the high mountains",
		}},
	})
	if estimate <= 0 {
		t.Fatalf("estimate = %d, want > 0", estimate)
	}
	// Two dispatches would have charged twice the estimate; a single logical
	// request charges once.
	if entry["day_tokens"] != estimate {
		t.Errorf("day_tokens = %d, want exactly one estimate (%d) — not one per dispatch", entry["day_tokens"], estimate)
	}
}

// TestKeyBudgetDenialIsTerminal pins that the caller's own daily budget fails
// the request without dispatching (a failover cannot help), while the provider
// budget skip is per target.
func TestKeyBudgetDenialIsTerminal(t *testing.T) {
	first, firstCalls := okUpstream(t)
	second, secondCalls := okUpstream(t)

	p, gw := budgetChainProxy(t, []config.ProviderConfig{
		{URL: first.URL, AuthStyle: "none"},
		{URL: second.URL, AuthStyle: "none"},
	}, config.ApiKey{TokenBudgetDaily: 1}, 600, 10_000_000)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, budgetRequest())
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "key_budget") {
		t.Errorf("expected the key_budget reason, got %s", rec.Body.String())
	}
	if firstCalls.Load()+secondCalls.Load() != 0 {
		t.Errorf("a key-budget denial must not dispatch anything (%d + %d)", firstCalls.Load(), secondCalls.Load())
	}
	var buf strings.Builder
	gw.Metrics.WritePrometheus(&buf)
	assertDenialMetric(t, buf.String(), "key_budget")
}

// TestMCPLoopChargesKeyBudget pins NENYA-188's coverage of the MCP path: the
// MCP loop dispatches upstream calls without going through the chat retry
// loop's key-budget charge, so it reserves the caller's daily budget itself —
// once per logical request, before the first dispatch, and a denial is terminal
// (429 key_budget with nothing dispatched).
func TestMCPLoopChargesKeyBudget(t *testing.T) {
	upstream, calls := sseUpstream(t)
	p, gw := budgetChainProxy(t, []config.ProviderConfig{
		{URL: upstream.URL, AuthStyle: "none"},
	}, config.ApiKey{TokenBudgetDaily: 1}, 600, 10_000_000)
	attachTestMCPClient(t, gw)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, budgetRequest())

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 from the MCP loop budget charge, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "key_budget") {
		t.Errorf("expected the key_budget reason, got %s", rec.Body.String())
	}
	if calls.Load() != 0 {
		t.Errorf("upstream dispatched %d times, want 0 (the budget charge precedes the first dispatch)", calls.Load())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type = %q, want text/event-stream (the MCP loop's wire shape)", ct)
	}
}

// TestMCPLoopServesWithinKeyBudget pins that a budgeted key whose estimate fits
// the daily budget is still served by the MCP loop (the charge is not a
// rejection path for normal traffic).
func TestMCPLoopServesWithinKeyBudget(t *testing.T) {
	upstream, calls := sseUpstream(t)
	p, gw := budgetChainProxy(t, []config.ProviderConfig{
		{URL: upstream.URL, AuthStyle: "none"},
	}, config.ApiKey{TokenBudgetDaily: 1_000_000}, 600, 10_000_000)
	attachTestMCPClient(t, gw)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, budgetRequest())

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 1 {
		t.Errorf("upstream dispatched %d times, want 1", calls.Load())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type = %q, want text/event-stream (the MCP loop replayed SSE)", ct)
	}
	keys, _ := gw.KeyUsage.Snapshot()["keys"].(map[string]interface{})
	entry, _ := keys["budget-key"].(map[string]int)
	if entry["day_tokens"] <= 0 {
		t.Errorf("the MCP loop must charge the key once: %v", keys)
	}
}

// attachTestMCPClient wires a mock MCP server's client and tool registry into
// the gateway so the MCP multi-turn loop runs for the agent.
func attachTestMCPClient(t *testing.T, gw *gateway.NenyaGateway) {
	t.Helper()
	mock := newTestMCPServer(t)
	client := mcp.NewClient(mcp.ClientConfig{
		Name:   "nenya-test",
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.RefreshTools(t.Context()); err != nil {
		t.Fatalf("RefreshTools failed: %v", err)
	}
	toolIndex := mcp.NewToolRegistry()
	toolIndex.Register("memory", client.ListTools())
	gw.MCPClients = map[string]*mcp.Client{"memory": client}
	gw.MCPToolIndex = toolIndex
	gw.Config.MCPServers = map[string]config.MCPServerConfig{"memory": {URL: mock.server.URL + "/sse"}}
	for name, agent := range gw.Config.Agents {
		agent.MCP = &config.AgentMCPConfig{Servers: []string{"memory"}}
		gw.Config.Agents[name] = agent
	}
}

// TestReserveKeyBudget pins the reservation helper: it charges the key's daily
// budget with the request estimate, is a no-op without a budgeted key, and
// reports the denial reason when the charge is refused (NENYA-188).
func TestReserveKeyBudget(t *testing.T) {
	gw := &gateway.NenyaGateway{KeyUsage: auth.NewKeyUsageTracker(nil)}

	if reason, denied := reserveKeyBudget(gw, nil, 100); denied || reason != "" {
		t.Fatalf("nil key: reason=%q denied=%v, want a no-op", reason, denied)
	}
	if reason, denied := reserveKeyBudget(gw, &config.ApiKey{Name: "unbudgeted"}, 100); denied {
		t.Fatalf("unbudgeted key: reason=%q, want a no-op", reason)
	}
	key := &config.ApiKey{Name: "k", TokenBudgetDaily: 100}
	if reason, denied := reserveKeyBudget(gw, key, 40); denied {
		t.Fatalf("first charge denied: %q", reason)
	}
	keys, _ := gw.KeyUsage.Snapshot()["keys"].(map[string]interface{})
	entry, _ := keys["k"].(map[string]int)
	if entry["day_tokens"] != 40 {
		t.Fatalf("day_tokens = %d, want 40", entry["day_tokens"])
	}
	// The reservation is non-refundable and cumulative.
	if reason, denied := reserveKeyBudget(gw, key, 40); denied {
		t.Fatalf("second charge denied: %q", reason)
	}
	if reason, denied := reserveKeyBudget(gw, key, 40); !denied || reason != "key_budget" {
		t.Fatalf("over-budget charge: reason=%q denied=%v, want key_budget/true", reason, denied)
	}
	// A refused charge does not consume budget.
	keys, _ = gw.KeyUsage.Snapshot()["keys"].(map[string]interface{})
	entry, _ = keys["k"].(map[string]int)
	if entry["day_tokens"] != 80 {
		t.Fatalf("day_tokens after a refused charge = %d, want 80", entry["day_tokens"])
	}
}

// assertDenialMetric asserts the denial counter carries the key name and reason
// (tolerant of label rendering order).
func assertDenialMetric(t *testing.T, metrics, reason string) {
	t.Helper()
	for _, line := range strings.Split(metrics, "\n") {
		if !strings.HasPrefix(line, "nenya_auth_denials_total{") {
			continue
		}
		if strings.Contains(line, `key_name="budget-key"`) && strings.Contains(line, `reason="`+reason+`"`) {
			return
		}
	}
	t.Errorf("expected nenya_auth_denials_total{key_name=\"budget-key\", reason=%q}, got:\n%s", reason, metrics)
}

// TestBudgetSkipDoesNotMaskALaterFailure pins the NENYA-188 reporting rule: a
// budget-skipped target must not turn a later target's real failure into a
// budget 429. The client keeps the provider error, and the budget skip only
// changes which target served.
func TestBudgetSkipDoesNotMaskALaterFailure(t *testing.T) {
	first, firstCalls := okUpstream(t)
	var secondCalls atomic.Int32
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream exploded"}}`))
	}))
	t.Cleanup(failing.Close)

	p, _ := budgetChainProxy(t, []config.ProviderConfig{
		{URL: first.URL, AuthStyle: "none", TokenBudgetDaily: 1},
		{URL: failing.URL, AuthStyle: "none"},
	}, config.ApiKey{TokenBudgetDaily: 10_000_000, BudgetTier: "fill"}, 600, 10_000_000)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, budgetRequest())

	if rec.Code == http.StatusTooManyRequests {
		t.Fatalf("a later target reached the provider and failed: got a budget 429 (%s) instead of its own error", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "provider_budget") {
		t.Errorf("the budget skip must not be reported as the outcome: %s", rec.Body.String())
	}
	if firstCalls.Load() != 0 {
		t.Errorf("budget-exhausted target was dispatched %d times, want 0", firstCalls.Load())
	}
	if secondCalls.Load() == 0 {
		t.Error("expected the second target to be dispatched")
	}
}

// TestBudgetSkipDoesNotMaskAnHTMLFailure pins the structural masking rule for
// the HTML-rejection path: an upstream that answered 200 with text/html was
// reached, so its skip must not be reported as a budget exhaustion.
func TestBudgetSkipDoesNotMaskAnHTMLFailure(t *testing.T) {
	first, firstCalls := okUpstream(t)
	var secondCalls atomic.Int32
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html><body>proxy interstitial</body></html>"))
	}))
	t.Cleanup(html.Close)

	p, _ := budgetChainProxy(t, []config.ProviderConfig{
		{URL: first.URL, AuthStyle: "none", TokenBudgetDaily: 1},
		{URL: html.URL, AuthStyle: "none"},
	}, config.ApiKey{TokenBudgetDaily: 10_000_000, BudgetTier: "fill"}, 600, 10_000_000)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, budgetRequest())

	if rec.Code == http.StatusTooManyRequests {
		t.Fatalf("an HTML response reached a provider: got a budget 429 (%s)", rec.Body.String())
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want the generic exhaustion 503 for the reached HTML target", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "provider_budget") {
		t.Errorf("the budget skip must not be reported as the outcome: %s", rec.Body.String())
	}
	if firstCalls.Load() != 0 {
		t.Errorf("budget-exhausted target was dispatched %d times, want 0", firstCalls.Load())
	}
	if secondCalls.Load() == 0 {
		t.Error("expected the second target to be dispatched")
	}
}

// TestAlwaysTierIgnoresExhaustedProviderBudget pins the tier rule: an `always`
// key (the default) is served even when the provider budget is exhausted, and
// the skip path does not charge provider budget for a target it skipped.
func TestAlwaysTierIgnoresExhaustedProviderBudget(t *testing.T) {
	first, firstCalls := okUpstream(t)
	second, secondCalls := okUpstream(t)

	p, gw := budgetChainProxy(t, []config.ProviderConfig{
		{URL: first.URL, AuthStyle: "none", TokenBudgetDaily: 1},
		{URL: second.URL, AuthStyle: "none", TokenBudgetDaily: 1},
	}, config.ApiKey{TokenBudgetDaily: 10_000_000}, 600, 10_000_000)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, budgetRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("always-tier key must be served, got %d: %s", rec.Code, rec.Body.String())
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Errorf("dispatches = %d/%d, want the first target only", firstCalls.Load(), secondCalls.Load())
	}
	// The `always` tier charges the provider budget for the target it serves;
	// the never-guarded second provider keeps no usage entry at all (the
	// skipped-provider-at-zero case is asserted in
	// TestProviderBudgetExhaustedTargetIsSkipped).
	providers, _ := gw.KeyUsage.Snapshot()["providers"].(map[string]interface{})
	if entry, ok := providers["p2"].(map[string]int); ok && entry["day_tokens"] != 0 {
		t.Errorf("provider p2 day_tokens = %d, want 0 (never dispatched)", entry["day_tokens"])
	}
}
