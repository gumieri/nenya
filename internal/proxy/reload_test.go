package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nenya/config"
	"nenya/internal/gateway"
)

func newReloadTestGateway(t *testing.T, upstreamURL string) *gateway.NenyaGateway {
	t.Helper()
	cfg := config.Config{
		Server: config.ServerConfig{
			MaxBodyBytes: 10 << 20,
		},
		Governance: config.GovernanceConfig{
			RatelimitMaxRPM: 60,
			RatelimitMaxTPM: 100000,
		},
		SecurityFilter: config.BouncerConfig{
			Enabled: false,
		},
		Providers: map[string]config.ProviderConfig{
			"test-provider": {
				URL:       upstreamURL + "/v1/chat/completions",
				AuthStyle: "none",
			},
		},
		Agents: map[string]config.AgentConfig{
			"test-model": {
				Strategy: "fallback",
				Models: []config.AgentModel{
					{Provider: "test-provider", Model: "test-model"},
				},
			},
		},
	}
	secrets := &config.SecretsConfig{
		ClientToken:  "test-token",
		ProviderKeys: map[string]string{},
	}
	return gateway.New(context.Background(), cfg, secrets, slog.Default())
}

func TestAuthenticateRequest_NilGateway(t *testing.T) {
	p := &Proxy{}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	_, ok := p.authenticateRequest(req, rec)
	if ok {
		t.Fatal("expected false for nil gateway")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestAuthenticateRequest_ReturnsKeyName(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("primary token returns primary", func(t *testing.T) {
		gw := gateway.New(context.Background(), config.Config{}, &config.SecretsConfig{
			ClientToken: "test-client-token",
			ApiKeys:     map[string]config.ApiKey{},
		}, logger)
		p := &Proxy{}
		p.StoreGateway(gw)
		defer p.StoreGateway(nil)

		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer test-client-token")
		rec := httptest.NewRecorder()

		keyRef, ok := p.authenticateRequest(req, rec)
		if !ok {
			t.Fatal("expected true for primary token")
		}
		if keyRef != "primary" {
			t.Fatalf("expected 'primary', got %q", keyRef)
		}
	})

	t.Run("named API key returns its name", func(t *testing.T) {
		gw := gateway.New(context.Background(), config.Config{}, &config.SecretsConfig{
			ClientToken: "",
			ApiKeys: map[string]config.ApiKey{
				"my-key": {
					Name:    "my-key",
					Token:   "super-secret-token",
					Roles:   []string{"user"},
					Enabled: true,
				},
			},
		}, logger)
		p := &Proxy{}
		p.StoreGateway(gw)
		defer p.StoreGateway(nil)

		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer super-secret-token")
		rec := httptest.NewRecorder()

		keyRef, ok := p.authenticateRequest(req, rec)
		if !ok {
			t.Fatal("expected true for named API key")
		}
		if keyRef != "my-key" {
			t.Fatalf("expected 'my-key', got %q", keyRef)
		}
	})

	t.Run("invalid token returns empty keyRef", func(t *testing.T) {
		gw := gateway.New(context.Background(), config.Config{}, &config.SecretsConfig{
			ClientToken: "valid-token",
			ApiKeys:     map[string]config.ApiKey{},
		}, logger)
		p := &Proxy{}
		p.StoreGateway(gw)
		defer p.StoreGateway(nil)

		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer invalid-token")
		rec := httptest.NewRecorder()

		keyRef, ok := p.authenticateRequest(req, rec)
		if ok {
			t.Fatal("expected false for invalid token")
		}
		if keyRef != "" {
			t.Fatalf("expected empty keyRef for invalid token, got %q", keyRef)
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", rec.Code)
		}
	})

	t.Run("missing Authorization header returns empty keyRef", func(t *testing.T) {
		gw := gateway.New(context.Background(), config.Config{}, &config.SecretsConfig{
			ClientToken: "valid-token",
		}, logger)
		p := &Proxy{}
		p.StoreGateway(gw)
		defer p.StoreGateway(nil)

		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		rec := httptest.NewRecorder()

		keyRef, ok := p.authenticateRequest(req, rec)
		if ok {
			t.Fatal("expected false for missing auth header")
		}
		if keyRef != "" {
			t.Fatalf("expected empty keyRef for missing header, got %q", keyRef)
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", rec.Code)
		}
	})
}

func TestServeHTTP_NilGateway(t *testing.T) {
	p := &Proxy{}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestStoreGateway_SwapConsistency(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw1 := gateway.New(context.Background(), config.Config{}, &config.SecretsConfig{ClientToken: "old"}, logger)
	gw2 := gateway.New(context.Background(), config.Config{}, &config.SecretsConfig{ClientToken: "new"}, logger)

	p := &Proxy{}
	p.StoreGateway(gw1)

	if p.Gateway() != gw1 {
		t.Fatal("expected gw1")
	}

	p.StoreGateway(gw2)

	if p.Gateway() != gw2 {
		t.Fatal("expected gw2 after swap")
	}
}

func TestHandleChatCompletions_GatewaySwapDuringRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")); err != nil {
			return
		}
		if _, err := w.Write([]byte("data: [DONE]\n\n")); err != nil {
			return
		}
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	gw1 := newReloadTestGateway(t, upstream.URL)
	gw2 := newReloadTestGateway(t, upstream.URL)

	p := &Proxy{}
	p.StoreGateway(gw1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		body := strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()

		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
		respBody := rec.Body.String()
		if !strings.Contains(respBody, "hello") {
			t.Errorf("expected 'hello' in response, got: %s", respBody)
		}
	}()

	p.StoreGateway(gw2)

	<-done
}
