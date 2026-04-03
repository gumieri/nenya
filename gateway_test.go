package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestGateway(cfg Config) *NenyaGateway {
	return &NenyaGateway{
		config:         cfg,
		secrets:        &SecretsConfig{},
		providers:      resolveProviders(&cfg, &SecretsConfig{}),
		rateLimits:     make(map[string]*rateLimiter),
		secretPatterns: nil,
		stats:          NewUsageTracker(),
		logger:         slog.Default(),
		agentCounters:  make(map[string]uint64),
		modelCooldowns: make(map[string]time.Time),
	}
}

func newTestGatewayWithLogger(cfg Config) *NenyaGateway {
	return NewNenyaGateway(cfg, &SecretsConfig{}, slog.Default())
}

func newAuthenticatedGateway(cfg Config, secrets *SecretsConfig) *NenyaGateway {
	if cfg.Server.MaxBodyBytes == 0 {
		cfg.Server.MaxBodyBytes = 10 << 20
	}
	return &NenyaGateway{
		config:         cfg,
		secrets:        secrets,
		providers:      resolveProviders(&cfg, secrets),
		rateLimits:     make(map[string]*rateLimiter),
		secretPatterns: nil,
		stats:          NewUsageTracker(),
		logger:         slog.Default(),
		client:         http.DefaultClient,
		agentCounters:  make(map[string]uint64),
		modelCooldowns: make(map[string]time.Time),
	}
}

func TestCountTokens(t *testing.T) {
	cfg := Config{}
	g := newTestGatewayWithLogger(cfg)

	text := "Hello, world! This is a test."
	tokens := g.countTokens(text)
	if tokens <= 0 {
		t.Errorf("Expected positive token count, got %d", tokens)
	}
}

func TestHealthz(t *testing.T) {
	g := newTestGateway(Config{})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != http.StatusOK && w.Code != http.StatusServiceUnavailable {
		t.Errorf("healthz: expected 200 or 503, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz: failed to parse JSON: %v", err)
	}
	if _, ok := resp["ollama"]; !ok {
		t.Error("healthz: missing ollama field")
	}
}

func TestModelsEndpoint(t *testing.T) {
	secrets := &SecretsConfig{ClientToken: "test-token", ProviderKeys: map[string]string{"gemini": "AIza-test"}}
	cfg := Config{
		Providers: builtInProviders(),
		Agents: map[string]AgentConfig{
			"code": {
				Models: []AgentModel{
					{Provider: "zai", Model: "zai-coding-plan/glm-5"},
					{Provider: "gemini", Model: "gemini-2.5-flash"},
				},
			},
			"fast": {
				Models: []AgentModel{
					{Provider: "zai", Model: "glm-4.7-flash"},
				},
			},
		},
	}
	g := newAuthenticatedGateway(cfg, secrets)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("models: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse models response: %v", err)
	}

	if resp.Object != "list" {
		t.Errorf("expected object='list', got %q", resp.Object)
	}

	seen := make(map[string]bool)
	for _, m := range resp.Data {
		if seen[m.ID] {
			t.Errorf("duplicate model: %s", m.ID)
		}
		seen[m.ID] = true

		switch m.ID {
		case "code":
			if m.OwnedBy != "nenya" {
				t.Errorf("agent 'code': expected owned_by='nenya', got %q", m.OwnedBy)
			}
		case "fast":
			if m.OwnedBy != "nenya" {
				t.Errorf("agent 'fast': expected owned_by='nenya', got %q", m.OwnedBy)
			}
		case "zai-coding-plan/glm-5":
			if m.OwnedBy != "zai" {
				t.Errorf("model 'zai-coding-plan/glm-5': expected owned_by='zai', got %q", m.OwnedBy)
			}
		}
	}

	if !seen["code"] {
		t.Error("missing agent 'code' in models list")
	}
	if !seen["fast"] {
		t.Error("missing agent 'fast' in models list")
	}
	if !seen["zai-coding-plan/glm-5"] {
		t.Error("missing model 'zai-coding-plan/glm-5' in models list")
	}
	if !seen["gemini-2.5-flash"] {
		t.Error("missing model 'gemini-2.5-flash' in models list")
	}
	if !seen["glm-4.7-flash"] {
		t.Error("missing model 'glm-4.7-flash' in models list")
	}
}

func TestModelsEndpointUnauthorized(t *testing.T) {
	g := newTestGateway(Config{})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestModelsDeduplicatesAgentAndProviderModels(t *testing.T) {
	secrets := &SecretsConfig{ClientToken: "tok", ProviderKeys: map[string]string{"zai": "z"}}
	cfg := Config{
		Providers: builtInProviders(),
		Agents: map[string]AgentConfig{
			"myagent": {
				Models: []AgentModel{
					{Provider: "zai", Model: "zai-coding-plan/glm-5"},
					{Provider: "zai", Model: "zai-coding-plan/glm-5"},
				},
			},
		},
	}
	g := newAuthenticatedGateway(cfg, secrets)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)

	count := 0
	for _, m := range resp.Data {
		if m.ID == "zai-coding-plan/glm-5" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 occurrence of zai-coding-plan/glm-5, got %d", count)
	}
}

func TestEmbeddingsEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("expected /v1/embeddings, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-api-key" {
			t.Errorf("missing API key in upstream request")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data":   []map[string]interface{}{},
			"model":  "text-embedding-3-small",
		})
	}))
	defer upstream.Close()

	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"openai": "test-api-key"},
	}
	cfg := Config{
		Providers: map[string]ProviderConfig{
			"openai": {
				URL:           upstream.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"text-embedding-", "gpt-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := newAuthenticatedGateway(cfg, secrets)

	body := `{"model": "text-embedding-3-small", "input": "hello world"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("embeddings: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("expected object='list', got %v", resp["object"])
	}
}

func TestEmbeddingsNoProvider(t *testing.T) {
	secrets := &SecretsConfig{ClientToken: "tok", ProviderKeys: map[string]string{"gemini": "k"}}
	cfg := Config{Providers: builtInProviders()}
	g := newAuthenticatedGateway(cfg, secrets)

	body := `{"model": "text-embedding-3-small", "input": "test"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown provider, got %d", w.Code)
	}
}

func TestEmbeddingsMissingModel(t *testing.T) {
	secrets := &SecretsConfig{ClientToken: "tok"}
	cfg := Config{Providers: builtInProviders()}
	g := newAuthenticatedGateway(cfg, secrets)

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"input": "test"}`))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing model, got %d", w.Code)
	}
}

func TestEmbeddingsUnauthorized(t *testing.T) {
	g := newTestGateway(Config{})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	g := newTestGateway(Config{})
	req := httptest.NewRequest(http.MethodDelete, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for DELETE /v1/chat/completions, got %d", w.Code)
	}
}

func TestNotFound(t *testing.T) {
	g := newTestGateway(Config{})
	req := httptest.NewRequest(http.MethodGet, "/v1/nonexistent", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestStatsEndpoint(t *testing.T) {
	g := newTestGateway(Config{})
	g.stats.RecordRequest("gemini-2.5-flash", 100)
	g.stats.RecordRequest("gemini-2.5-flash", 200)
	g.stats.RecordOutput("gemini-2.5-flash", 50)
	g.stats.RecordError("deepseek-v3")

	req := httptest.NewRequest(http.MethodGet, "/statsz", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("statsz: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Uptime int `json:"uptime_seconds"`
		Models map[string]struct {
			Requests     uint64 `json:"requests"`
			InputTokens  uint64 `json:"input_tokens"`
			OutputTokens uint64 `json:"output_tokens"`
			Errors       uint64 `json:"errors"`
		} `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse stats: %v", err)
	}

	flash, ok := resp.Models["gemini-2.5-flash"]
	if !ok {
		t.Fatal("missing gemini-2.5-flash in stats")
	}
	if flash.Requests != 2 {
		t.Errorf("expected 2 requests, got %d", flash.Requests)
	}
	if flash.InputTokens != 300 {
		t.Errorf("expected 300 input tokens, got %d", flash.InputTokens)
	}
	if flash.OutputTokens != 50 {
		t.Errorf("expected 50 output tokens, got %d", flash.OutputTokens)
	}

	ds, ok := resp.Models["deepseek-v3"]
	if !ok {
		t.Fatal("missing deepseek-v3 in stats")
	}
	if ds.Errors != 1 {
		t.Errorf("expected 1 error, got %d", ds.Errors)
	}
}
