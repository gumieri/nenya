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
	"nenya/internal/testutil"
)

func TestHandleChatCompletions_EmptyUpstreamStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = 60
	cfg.Governance.RatelimitMaxTPM = 100000
	cfg.Governance.EmptyStreamAsError = true
	cfg.SecurityFilter.Enabled = false
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider": {
			URL:       upstream.URL + "/v1/chat/completions",
			AuthStyle: "none",
		},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models: []config.AgentModel{
				{Provider: "test-provider", Model: "test-model"},
			},
		},
	}
	secrets := &config.SecretsConfig{
		ClientToken: "test-token",
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)

	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	respBody, _ := io.ReadAll(rec.Body)
	respStr := string(respBody)

	if !strings.Contains(respStr, `"empty_response"`) {
		t.Errorf("expected response to contain empty_response error, got: %s", respStr)
	}
	if !strings.Contains(respStr, `"empty upstream SSE"`) {
		t.Errorf("expected response to contain error message, got: %s", respStr)
	}
	if !strings.Contains(respStr, "data: [DONE]") {
		t.Errorf("expected response to contain [DONE] sentinel, got: %s", respStr)
	}
}

func TestHandleChatCompletions_EmptyUpstreamStream_FlagDisabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = 60
	cfg.Governance.RatelimitMaxTPM = 100000
	cfg.Governance.EmptyStreamAsError = false
	cfg.SecurityFilter.Enabled = false
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider": {
			URL:       upstream.URL + "/v1/chat/completions",
			AuthStyle: "none",
		},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models: []config.AgentModel{
				{Provider: "test-provider", Model: "test-model"},
			},
		},
	}
	secrets := &config.SecretsConfig{
		ClientToken: "test-token",
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)

	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	respBody, _ := io.ReadAll(rec.Body)
	respStr := string(respBody)

	if strings.Contains(respStr, `"empty_response"`) {
		t.Errorf("expected no empty_response error when flag disabled, got: %s", respStr)
	}
}

func TestHandleChatCompletions_EmptyUpstreamStream_RecordsMetric(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = 60
	cfg.Governance.RatelimitMaxTPM = 100000
	cfg.Governance.EmptyStreamAsError = true
	cfg.SecurityFilter.Enabled = false
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider": {
			URL:       upstream.URL + "/v1/chat/completions",
			AuthStyle: "none",
		},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models: []config.AgentModel{
				{Provider: "test-provider", Model: "test-model"},
			},
		},
	}
	secrets := &config.SecretsConfig{
		ClientToken: "test-token",
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)

	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	var out strings.Builder
	gw.Metrics.WritePrometheus(&out)
	metricsOutput := out.String()

	if !strings.Contains(metricsOutput, `nenya_empty_stream_total`) {
		t.Errorf("expected empty_stream metric to be recorded, got:\n%s", metricsOutput)
	}
	if !strings.Contains(metricsOutput, `model="test-model"`) {
		t.Errorf("expected test-model label in metric, got:\n%s", metricsOutput)
	}
	if !strings.Contains(metricsOutput, `provider="test-provider"`) {
		t.Errorf("expected test-provider label in metric, got:\n%s", metricsOutput)
	}
}
