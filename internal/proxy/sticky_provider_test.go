package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
)

// stickyProxy builds a gateway whose agent lists the same upstream twice —
// the operator-configured failover-within-provider idiom — with the given
// sticky_provider policy.
func stickyProxy(t *testing.T, sticky string, upstreamURL string) *Proxy {
	t.Helper()
	cfg := config.Config{
		Server: config.ServerConfig{MaxBodyBytes: 10 << 20},
		Governance: config.GovernanceConfig{
			RatelimitMaxRPM: config.PtrTo(60),
			RatelimitMaxTPM: config.PtrTo(100000),
		},
		Bouncer: config.BouncerConfig{Enabled: config.PtrTo(false)},
		Providers: map[string]config.ProviderConfig{
			"test-provider": {
				URL:       upstreamURL + "/v1/chat/completions",
				AuthStyle: "none",
			},
		},
		Agents: map[string]config.AgentConfig{
			"sticky-agent": {
				Models: []config.AgentModel{
					{Provider: "test-provider", Model: "test-model"},
					{Provider: "test-provider", Model: "test-model"},
				},
				StickyProvider: sticky,
			},
		},
	}
	secrets := &config.SecretsConfig{ClientToken: "test-token", ProviderKeys: map[string]string{}}
	gw := newStickyTestGateway(t, cfg, secrets)
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

func newStickyTestGateway(t *testing.T, cfg config.Config, secrets *config.SecretsConfig) *gateway.NenyaGateway {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return gateway.New(context.Background(), cfg, secrets, logger)
}

func doStickyRequest(t *testing.T, p *Proxy) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"sticky-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func TestStickyProvider_OffFailsOverOn5xx(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	rec := doStickyRequest(t, stickyProxy(t, "", upstream.URL))
	if rec.Code != http.StatusOK {
		t.Fatalf("default policy must fail over on 5xx: got %d", rec.Code)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("expected 2 upstream hits, got %d", got)
	}
}

func TestStickyProvider_StrictBlocksFailover(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	rec := doStickyRequest(t, stickyProxy(t, "strict", upstream.URL))
	// Strict blocks failover, including to the duplicate same-target entry
	// (the repeat is not attempted because it is not the final sweep index in
	// this chain layout): one dispatch, then the typed exhaustion error.
	if got := attempts.Load(); got != 1 {
		t.Fatalf("strict must block the second dispatch, got %d hits", got)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("strict expects typed exhausted error, got %d", rec.Code)
	}
	assertErrorKind(t, rec, "provider_error")
}

func TestStickyProvider_LenientAllows5xxBlocks4xx(t *testing.T) {
	// 5xx: lenient fails over.
	var attempts atomic.Int32
	up5xx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"bad gateway"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`))
	}))
	defer up5xx.Close()

	rec := doStickyRequest(t, stickyProxy(t, "lenient", up5xx.URL))
	if rec.Code != http.StatusOK || attempts.Load() != 2 {
		t.Fatalf("lenient must fail over on 5xx: code=%d hits=%d", rec.Code, attempts.Load())
	}

	// 4xx (no retryable phrase configured): lenient stays on the failed
	// target — the sweep is blocked and the request exhausts without a
	// second dispatch.
	attempts4xx := atomic.Int32{}
	up4xx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts4xx.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request shape"}}`))
	}))
	defer up4xx.Close()

	rec = doStickyRequest(t, stickyProxy(t, "lenient", up4xx.URL))
	if got := attempts4xx.Load(); got != 1 {
		t.Fatalf("lenient must not fail over on 4xx, got %d hits", got)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected blocked sweep to exhaust with 503, got %d", rec.Code)
	}
}

// TestModelAlias_UpstreamReceivesAliasedID pins NENYA-22 end to end: the
// client sends the canonical ID, the provider's model_aliases entry rewrites
// it at dispatch, and the upstream observes the physical ID.
func TestModelAlias_UpstreamReceivesAliasedID(t *testing.T) {
	var observed atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		observed.Store(body.Model)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{MaxBodyBytes: 10 << 20},
		Governance: config.GovernanceConfig{
			RatelimitMaxRPM: config.PtrTo(60),
			RatelimitMaxTPM: config.PtrTo(100000),
		},
		Bouncer: config.BouncerConfig{Enabled: config.PtrTo(false)},
		Providers: map[string]config.ProviderConfig{
			"test-provider": {
				URL:           upstream.URL + "/v1/chat/completions",
				AuthStyle:     "none",
				ModelAliases:  map[string]string{"claude-haiku-4.5": "claude-haiku-4-5"},
				AllowedModels: []string{"^claude"},
			},
		},
		Agents: map[string]config.AgentConfig{
			"alias-agent": {
				Models: []config.AgentModel{
					{Provider: "test-provider", Model: "claude-haiku-4.5"},
				},
			},
		},
	}
	secrets := &config.SecretsConfig{ClientToken: "test-token", ProviderKeys: map[string]string{}}
	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	gw := gateway.New(context.Background(), cfg, secrets, logger)
	p := &Proxy{}
	p.StoreGateway(gw)

	body := `{"model":"alias-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s (log: %s)", rec.Code, rec.Body.String(), logBuf.String())
	}
	if got, _ := observed.Load().(string); got != "claude-haiku-4-5" {
		t.Fatalf("upstream observed model %q, want aliased claude-haiku-4-5 (log: %s)", got, logBuf.String())
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// newBudgetUpstream returns a healthy non-streaming upstream.
func newBudgetUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`))
	}))
}

// TestKeyRateLimit_SecondRequestDenied pins NENYA-20 auth-side enforcement:
// a key with ratelimit_max_rpm=1 gets a typed 429 on the second request.
func TestKeyRateLimit_SecondRequestDenied(t *testing.T) {
	upstream := newBudgetUpstream()
	defer upstream.Close()

	cfg := config.Config{
		Server:     config.ServerConfig{MaxBodyBytes: 10 << 20},
		Governance: config.GovernanceConfig{RatelimitMaxRPM: config.PtrTo(60), RatelimitMaxTPM: config.PtrTo(100000)},
		Bouncer:    config.BouncerConfig{Enabled: config.PtrTo(false)},
		Providers: map[string]config.ProviderConfig{
			"test-provider": {URL: upstream.URL + "/v1/chat/completions", AuthStyle: "none"},
		},
		Agents: map[string]config.AgentConfig{
			"budget-agent": {Models: []config.AgentModel{{Provider: "test-provider", Model: "test-model"}}},
		},
	}
	secrets := &config.SecretsConfig{
		ClientToken:  "test-token",
		ProviderKeys: map[string]string{},
		ApiKeys: map[string]config.ApiKey{
			"limited": {Name: "limited", Token: "nk-limited-key-123456", Roles: []string{"user"}, Enabled: true, RatelimitMaxRPM: 1},
		},
	}
	gw := newStickyTestGateway(t, cfg, secrets)
	p := &Proxy{}
	p.StoreGateway(gw)

	doReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"budget-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer nk-limited-key-123456")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	if rec := doReq(); rec.Code != http.StatusOK {
		t.Fatalf("first request should pass, got %d: %s", rec.Code, rec.Body.String())
	}
	rec := doReq()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request should be 429, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rate limit exceeded") {
		t.Errorf("expected rate-limit message, got: %s", rec.Body.String())
	}
}

// TestKeyAndProviderBudgets pins NENYA-20 dispatch-side enforcement: a
// provider budget denies "fill"-tier keys while "always" keys keep working.
func TestKeyAndProviderBudgets(t *testing.T) {
	newGW := func(providers map[string]config.ProviderConfig) *Proxy {
		cfg := config.Config{
			Server:     config.ServerConfig{MaxBodyBytes: 10 << 20},
			Governance: config.GovernanceConfig{RatelimitMaxRPM: config.PtrTo(600), RatelimitMaxTPM: config.PtrTo(10000000)},
			Bouncer:    config.BouncerConfig{Enabled: config.PtrTo(false)},
			Providers:  providers,
			Agents: map[string]config.AgentConfig{
				"budget-agent": {Models: []config.AgentModel{{Provider: "test-provider", Model: "test-model"}}},
			},
		}
		secrets := &config.SecretsConfig{
			ClientToken:  "test-token",
			ProviderKeys: map[string]string{},
			ApiKeys: map[string]config.ApiKey{
				"fill-key":   {Name: "fill-key", Token: "nk-fill-key-123456789", Roles: []string{"user"}, Enabled: true, BudgetTier: "fill"},
				"always-key": {Name: "always-key", Token: "nk-always-key-12345678", Roles: []string{"user"}, Enabled: true},
			},
		}
		gw := newStickyTestGateway(t, cfg, secrets)
		p := &Proxy{}
		p.StoreGateway(gw)
		return p
	}

	t.Run("provider budget: fill denied, always served", func(t *testing.T) {
		upstream := newBudgetUpstream()
		defer upstream.Close()
		providers := map[string]config.ProviderConfig{
			"test-provider": {
				URL:              upstream.URL + "/v1/chat/completions",
				AuthStyle:        "none",
				TokenBudgetDaily: 1, // budget of 1 token; the request's estimate exceeds it
			},
		}
		p := newGW(providers)

		doLong := func(p *Proxy, token string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"budget-agent","stream":false,"messages":[{"role":"user","content":"please tell me a fairly long story about dragons and castles"}]}`))
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			return rec
		}

		rec := doLong(p, "nk-fill-key-123456789")
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("fill key should be 429 on exhausted provider budget, got %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "provider_budget") {
			t.Errorf("expected provider_budget reason, got: %s", rec.Body.String())
		}

		rec = doLong(p, "nk-always-key-12345678")
		if rec.Code != http.StatusOK {
			t.Fatalf("always key should be served despite exhaustion, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}
