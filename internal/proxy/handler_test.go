package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nenya/internal/config"
	"nenya/internal/gateway"
	"nenya/internal/testutil"
)

func newTestProxy(t *testing.T) (*Proxy, *httptest.Server) {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.SecurityFilter.Engine = config.EngineRef{
		Provider: "ollama",
		Model:    "qwen2.5-coder",
	}
	secrets := &config.SecretsConfig{
		ClientToken:  "test-token",
		ProviderKeys: map[string]string{"gemini": "test-key"},
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)
	return p, nil
}

func TestServeHTTP_Healthz_NoAuth(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON body, got error: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("expected status ok, got %v", body["status"])
	}
}

func TestServeHTTP_Statsz_NoAuth(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/statsz", nil)
	req.Header.Set("Authorization", "")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusUnauthorized)
}

func TestServeHTTP_Statsz_ValidAuth(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/statsz", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON body, got error: %v", err)
	}
}

func TestServeHTTP_Models_ValidAuth(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON body, got error: %v", err)
	}
	if body["object"] != "list" {
		t.Errorf("expected object=list, got %v", body["object"])
	}
}

func TestServeHTTP_Models_NoAuth(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusUnauthorized)
}

func TestServeHTTP_Models_WrongToken(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusForbidden)
}

func TestServeHTTP_ChatCompletions_NoAuth(t *testing.T) {
	p, _ := newTestProxy(t)
	body := `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", "")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusUnauthorized)
}

func TestServeHTTP_ChatCompletions_WrongToken(t *testing.T) {
	p, _ := newTestProxy(t)
	body := `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusForbidden)
}

func TestServeHTTP_UnknownPath(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/unknown", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusNotFound)
}

func TestServeHTTP_Models_WrongMethod(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/models", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusMethodNotAllowed)
}

func TestServeHTTP_Metrics_ValidAuth(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
}

func TestServeHTTP_Models_NoDeepSeekWithoutAPIKey(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON body, got error: %v, body: %s", err, rec.Body.String())
	}
	data, ok := body["data"].([]interface{})
	if !ok {
		t.Fatalf("expected data array, got body: %+v", body)
	}
	for _, m := range data {
		entry, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		id, _ := entry["id"].(string)
		ownedBy, _ := entry["owned_by"].(string)
		if strings.Contains(strings.ToLower(id), "deepseek") || strings.Contains(strings.ToLower(ownedBy), "deepseek") {
			t.Errorf("deepseek model should not appear without API key: id=%q ownedBy=%q", id, ownedBy)
		}
	}
}

func TestCheckOllamaProviderHealth_RetryOnce(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"models": []interface{}{}})
	}))
	defer server.Close()

	cfg := testutil.MinimalConfig()
	cfg.SecurityFilter.Engine = config.EngineRef{
		Provider: "ollama",
		Model:    "qwen2.5-coder",
	}
	secrets := &config.SecretsConfig{
		ClientToken: "test-token",
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())

	p := &Proxy{}
	p.StoreGateway(gw)

	ctx := context.Background()
	result := p.checkOllamaProviderHealth(ctx, gw, server.URL)
	if !result {
		t.Errorf("expected true after retry, got false")
	}
	if attempts.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts.Load())
	}
}

func TestCheckOllamaProviderHealth_AllFailed(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	cfg := testutil.MinimalConfig()
	cfg.SecurityFilter.Engine = config.EngineRef{
		Provider: "ollama",
		Model:    "qwen2.5-coder",
	}
	secrets := &config.SecretsConfig{
		ClientToken: "test-token",
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())

	p := &Proxy{}
	p.StoreGateway(gw)

	ctx := context.Background()
	result := p.checkOllamaProviderHealth(ctx, gw, server.URL)
	if result {
		t.Errorf("expected false when all attempts fail, got true")
	}
	if attempts.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts.Load())
	}
}

func TestCheckOllamaProviderHealth_ContextDeadline(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"models": []interface{}{}})
	}))
	defer server.Close()

	cfg := testutil.MinimalConfig()
	cfg.SecurityFilter.Engine = config.EngineRef{
		Provider: "ollama",
		Model:    "qwen2.5-coder",
	}
	secrets := &config.SecretsConfig{
		ClientToken: "test-token",
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())

	p := &Proxy{}
	p.StoreGateway(gw)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	result := p.checkOllamaProviderHealth(ctx, gw, server.URL)
	if result {
		t.Errorf("expected false due to context timeout, got true")
	}
	if attempts.Load() > 2 {
		t.Errorf("expected at most 2 attempts due to timeout, got %d", attempts.Load())
	}
}
