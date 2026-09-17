package proxy

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
	if got := attempts.Load(); got != 1 {
		t.Fatalf("strict must block the second dispatch, got %d hits", got)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("strict expects typed exhausted error, got %d", rec.Code)
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not structured: %v", err)
	}
	if body.Error.Type != "provider_error" {
		t.Errorf("expected provider_error type, got %q", body.Error.Type)
	}
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
