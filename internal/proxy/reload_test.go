package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nenya/internal/config"
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
		SecurityFilter: config.SecurityFilterConfig{
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

	ok := p.authenticateRequest(req, rec)
	if ok {
		t.Fatal("expected false for nil gateway")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
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
