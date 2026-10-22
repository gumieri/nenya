package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/testutil"
)

// reloadWindowSecretKey is the provider credential the validating upstream
// demands; a request dispatched with anything else is refused with 401.
const reloadWindowSecretKey = "sk-live-window-key"

// newReloadWindowProxy builds a proxy whose upstream validates the provider
// bearer token and records every Authorization value it saw.
func newReloadWindowProxy(t *testing.T) (*Proxy, *[]string, *sync.Mutex) {
	t.Helper()

	var mu sync.Mutex
	var authSeen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authSeen = append(authSeen, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+reloadWindowSecretKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := testutil.MinimalConfig()
	cfg.Providers = map[string]config.ProviderConfig{
		"prov": {
			URL:       upstream.URL + "/v1/chat/completions",
			AuthStyle: "bearer",
		},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models:   []config.AgentModel{{Provider: "prov", Model: "test-model"}},
		},
	}
	secrets := &config.SecretsConfig{
		ClientToken:  "test-token",
		ProviderKeys: map[string]string{"prov": reloadWindowSecretKey},
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(gw.Close)

	p := &Proxy{}
	p.StoreGateway(gw)
	return p, &authSeen, &mu
}

func runReloadWindowRequest(t *testing.T, p *Proxy) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// TestReloadSwapWindowServesFromOldGateway pins the AUD-07 fix (NENYA-215):
// between Reload returning and the caller storing the new gateway, the proxy
// still resolves — and must still serve from — the OLD gateway. Reload used
// to close it in that window: SecureMem was destroyed, so the provider
// credential read came back empty and the upstream saw a broken Authorization
// header (401 surfaced as a gateway error). Requests landing in the window
// must complete with the intact credential, and the swapped-out gateway's
// resources must close only via the post-swap drain.
func TestReloadSwapWindowServesFromOldGateway(t *testing.T) {
	p, authSeen, mu := newReloadWindowProxy(t)
	g1 := p.Gateway()

	// The production sequence up to the swap: Reload builds the replacement
	// without touching the serving gateway.
	g2 := g1.Reload(context.Background(), g1.Config, g1.Secrets)

	// The swap window: ServeHTTP still loads g1 here. Pre-fix this request
	// was served from a closed gateway (destroyed SecureMem → empty bearer
	// → upstream 401).
	rec := runReloadWindowRequest(t, p)
	if rec.Code != http.StatusOK {
		t.Fatalf("request in the reload swap window failed: status %d, body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	saw := len(*authSeen) > 0 && (*authSeen)[len(*authSeen)-1] == "Bearer "+reloadWindowSecretKey
	mu.Unlock()
	if !saw {
		t.Fatalf("upstream did not see the intact provider credential in the swap window: %v", *authSeen)
	}

	// Post-swap disposal (the new production sequence): the drained gateway
	// refuses admission — this is the 503 path a racing request hits.
	g1.CloseWhenDrained(context.Background())
	if !g1.Draining() {
		t.Fatal("the swapped-out gateway is not draining after CloseWhenDrained")
	}
	if g1.EnterRequest() {
		t.Fatal("the swapped-out gateway still admits requests after draining")
	}
	raceProxy := &Proxy{}
	raceProxy.StoreGateway(g1)
	recDrain := runReloadWindowRequest(t, raceProxy)
	if recDrain.Code != http.StatusServiceUnavailable {
		t.Fatalf("a request resolving the drained gateway must be refused with 503, got %d: %s", recDrain.Code, recDrain.Body.String())
	}

	// The proxy serves the new gateway normally after the swap.
	p.StoreGateway(g2)
	t.Cleanup(g2.Close)
	recNew := runReloadWindowRequest(t, p)
	if recNew.Code != http.StatusOK {
		t.Fatalf("request on the reloaded gateway failed: status %d, body %s", recNew.Code, recNew.Body.String())
	}
}

// TestReloadDrainHealthProbeExempt pins the /healthz exemption: the drain
// refusal must not fail the orchestrator's liveness probe (a 503 on a probe
// window comparable to the drain bound would get the pod killed mid-drain,
// defeating the drain), while every other route — including POST /healthz —
// is refused.
func TestReloadDrainHealthProbeExempt(t *testing.T) {
	p, _, _ := newReloadWindowProxy(t)
	g1 := p.Gateway()

	g1.CloseWhenDrained(context.Background())

	health := testutil.NewTestRequest(t, http.MethodGet, "/healthz", nil)
	recHealth := httptest.NewRecorder()
	p.ServeHTTP(recHealth, health)
	// Positive pin: the HEALTHZ handler served the request (its engine
	// payload), not a drain refusal.
	if !strings.Contains(recHealth.Body.String(), `"engine"`) {
		t.Fatalf("GET /healthz must be served by the health handler during drain, got %d: %s", recHealth.Code, recHealth.Body.String())
	}

	healthPost := testutil.NewTestRequest(t, http.MethodPost, "/healthz", nil)
	recPost := httptest.NewRecorder()
	p.ServeHTTP(recPost, healthPost)
	if recPost.Code != http.StatusServiceUnavailable || !strings.Contains(recPost.Body.String(), "draining") {
		t.Fatalf("POST /healthz is not a probe and must be refused during drain, got %d: %s", recPost.Code, recPost.Body.String())
	}

	chat := runReloadWindowRequest(t, p)
	if chat.Code != http.StatusServiceUnavailable {
		t.Fatalf("a chat request on a draining gateway must be refused with 503, got %d", chat.Code)
	}
}

// TestReloadSwapWindowInflightDrains pins the hold side of the fix: an
// in-flight request admitted before the reload still completes on the old
// gateway, and the drain's close waits for it — disposal must not run under
// a live request (pre-fix, Close destroyed SecureMem and closed the MCP
// clients at Reload time regardless of what was in flight).
func TestReloadSwapWindowInflightDrains(t *testing.T) {
	var mu sync.Mutex
	authSeen := false
	release := make(chan struct{})
	entered := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authSeen = r.Header.Get("Authorization") == "Bearer "+reloadWindowSecretKey
		mu.Unlock()
		close(entered) // the request is in flight on the old gateway
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	cfg := testutil.MinimalConfig()
	cfg.Providers = map[string]config.ProviderConfig{
		"prov": {URL: upstream.URL + "/v1/chat/completions", AuthStyle: "bearer"},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models:   []config.AgentModel{{Provider: "prov", Model: "test-model"}},
		},
	}
	secrets := &config.SecretsConfig{
		ClientToken:  "test-token",
		ProviderKeys: map[string]string{"prov": reloadWindowSecretKey},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	g1 := gateway.New(context.Background(), *cfg, secrets, logger)
	t.Cleanup(g1.Close)

	p := &Proxy{}
	p.StoreGateway(g1)

	// The request is admitted (and dispatches to the upstream) BEFORE the
	// reload starts — the production race shape.
	recDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := runReloadWindowRequest(t, p)
		recDone <- rec
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the upstream")
	}

	// Reload + arm the drain while the request is in flight.
	g2 := g1.Reload(context.Background(), g1.Config, g1.Secrets)
	t.Cleanup(func() { g2.Close() })
	drained := make(chan struct{})
	go func() {
		g1.CloseWhenDrained(context.Background())
		close(drained)
	}()

	// The close must wait for the in-flight request, not run under it.
	select {
	case <-drained:
		t.Fatal("CloseWhenDrained closed the gateway under the in-flight request")
	case <-time.After(150 * time.Millisecond):
	}

	// Release the upstream: the request completes with the intact
	// credential, and only then does the drain's close run.
	close(release)
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-recDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight request never completed")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("the in-flight request failed across the reload: status %d, body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	saw := authSeen
	mu.Unlock()
	if !saw {
		t.Fatal("the in-flight request's upstream call lost its provider credential")
	}

	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("the drain did not complete after the request exited")
	}
}
