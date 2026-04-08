package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{200, false},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{422, false},
		{429, true},
		{500, true},
		{502, true},
		{503, true},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			got := isRetryable(tt.status)
			if got != tt.want {
				t.Errorf("isRetryable(%d) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestOllamaHealthURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"standard generate URL", "http://127.0.0.1:11434/api/generate", "http://127.0.0.1:11434/api/tags"},
		{"custom port", "http://localhost:11435/api/generate", "http://localhost:11435/api/tags"},
		{"without trailing suffix", "http://127.0.0.1:11434/api/version", "http://127.0.0.1:11434/api/version"},
		{"non-standard path", "http://127.0.0.1:11434/v1/generate", "http://127.0.0.1:11434/v1/generate"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ollamaHealthURL(tt.url)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckSecurityFilterEngineHealth(t *testing.T) {
	t.Run("ollama reachable", func(t *testing.T) {
		ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/tags" {
				t.Errorf("expected /api/tags, got %s", r.URL.Path)
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer ollama.Close()

		cfg := Config{
			Providers: map[string]ProviderConfig{
				"ollama-health": {URL: ollama.URL + "/v1/chat/completions", AuthStyle: "none", ApiFormat: "ollama"},
			},
			SecurityFilter: SecurityFilterConfig{Engine: EngineConfig{Provider: "ollama-health", Model: "qwen2.5-coder:7b"}},
		}
		g := NewNenyaGateway(cfg, &SecretsConfig{}, slog.Default())

		if !g.checkSecurityFilterEngineHealth() {
			t.Error("expected healthy when Ollama returns 200")
		}
	})

	t.Run("ollama unreachable", func(t *testing.T) {
		cfg := Config{
			Providers: map[string]ProviderConfig{
				"ollama-bad": {URL: "http://127.0.0.1:1/api/generate", AuthStyle: "none", ApiFormat: "ollama"},
			},
			SecurityFilter: SecurityFilterConfig{Engine: EngineConfig{Provider: "ollama-bad"}},
		}
		g := NewNenyaGateway(cfg, &SecretsConfig{}, slog.Default())

		if g.checkSecurityFilterEngineHealth() {
			t.Error("expected unhealthy when Ollama is unreachable")
		}
	})

	t.Run("openai engine always healthy", func(t *testing.T) {
		cfg := Config{
			Providers: map[string]ProviderConfig{
				"gemini": {URL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", AuthStyle: "bearer+x-goog", ApiFormat: "openai"},
			},
			SecurityFilter: SecurityFilterConfig{Engine: EngineConfig{Provider: "gemini"}},
		}
		g := NewNenyaGateway(cfg, &SecretsConfig{}, slog.Default())

		if !g.checkSecurityFilterEngineHealth() {
			t.Error("expected healthy for non-ollama engine")
		}
	})

	t.Run("nil client", func(t *testing.T) {
		cfg := Config{
			SecurityFilter: SecurityFilterConfig{Engine: EngineConfig{Provider: "nonexistent"}},
		}
		g := NewNenyaGateway(cfg, &SecretsConfig{}, slog.Default())

		if g.checkSecurityFilterEngineHealth() {
			t.Error("expected unhealthy when provider not found")
		}
	})
}

func TestHandleChatCompletionsTier1(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("failed to decode request body: %v", err)
			return
		}

		msgs := body["messages"].([]interface{})
		lastMsg := msgs[len(msgs)-1].(map[string]interface{})
		content := lastMsg["content"].(string)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"choices":[{"delta":{"content":"response for ` + content + `"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server:         ServerConfig{MaxBodyBytes: 1 << 20},
		Governance:     GovernanceConfig{ContextSoftLimit: 4000, ContextHardLimit: 24000},
		SecurityFilter: SecurityFilterConfig{Enabled: false},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           upstream.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"test-small","messages":[{"role":"user","content":"` + "hello" + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestForwardToUpstreamRetryable(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"choices":[{"delta":{"content":"ok"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 1 << 20},
		Agents: map[string]AgentConfig{
			"my-agent": {
				Strategy:        "fallback",
				CooldownSeconds: 60,
				Models: []AgentModel{
					{Provider: "test", Model: "test-model-1"},
					{Provider: "test", Model: "test-model-2"},
				},
			},
		},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           upstream.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"my-agent","messages":[{"role":"user","content":"retry me"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after retry, got %d: %s", w.Code, w.Body.String())
	}
	if callCount != 2 {
		t.Errorf("expected 2 upstream calls, got %d", callCount)
	}
}

func TestForwardToUpstreamNonRetryable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer upstream.Close()

	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 1 << 20},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           upstream.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"test-small","messages":[{"role":"user","content":"fail"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleEmbeddingsUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"upstream fail"}`))
	}))
	defer upstream.Close()

	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 1 << 20},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           upstream.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"embed-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"embed-test","input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 (passthrough from upstream), got %d: %s", w.Code, w.Body.String())
	}
}

func TestCallOllama(t *testing.T) {
	t.Run("successful response", func(t *testing.T) {
		ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"response": "hello summary",
			})
		}))
		defer ollama.Close()

		cfg := Config{
			Providers: map[string]ProviderConfig{
				"ollama-test": {URL: ollama.URL, AuthStyle: "none", ApiFormat: "ollama"},
			},
			SecurityFilter: SecurityFilterConfig{Engine: EngineConfig{Provider: "ollama-test", Model: "test"}},
		}
		g := NewNenyaGateway(cfg, &SecretsConfig{}, slog.Default())

		result, err := g.callEngine(context.Background(), g.config.SecurityFilter.Engine, "system prompt", "input text")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != "hello summary" {
			t.Errorf("got %q, want %q", result, "hello summary")
		}
	})

	t.Run("non-200 status", func(t *testing.T) {
		ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("internal error"))
		}))
		defer ollama.Close()

		cfg := Config{
			Providers: map[string]ProviderConfig{
				"ollama-test": {URL: ollama.URL, AuthStyle: "none", ApiFormat: "ollama"},
			},
			SecurityFilter: SecurityFilterConfig{Engine: EngineConfig{Provider: "ollama-test", Model: "test"}},
		}
		g := NewNenyaGateway(cfg, &SecretsConfig{}, slog.Default())

		_, err := g.callEngine(context.Background(), g.config.SecurityFilter.Engine, "sys", "input")
		if err == nil {
			t.Fatal("expected error for 500 status")
		}
	})

	t.Run("malformed JSON response", func(t *testing.T) {
		ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`not json`))
		}))
		defer ollama.Close()

		cfg := Config{
			Providers: map[string]ProviderConfig{
				"ollama-test": {URL: ollama.URL, AuthStyle: "none", ApiFormat: "ollama"},
			},
			SecurityFilter: SecurityFilterConfig{Engine: EngineConfig{Provider: "ollama-test", Model: "test"}},
		}
		g := NewNenyaGateway(cfg, &SecretsConfig{}, slog.Default())

		_, err := g.callEngine(context.Background(), g.config.SecurityFilter.Engine, "sys", "input")
		if err == nil {
			t.Fatal("expected error for malformed JSON")
		}
	})

	t.Run("missing response field", func(t *testing.T) {
		ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"other":"data"}`))
		}))
		defer ollama.Close()

		cfg := Config{
			Providers: map[string]ProviderConfig{
				"ollama-test": {URL: ollama.URL, AuthStyle: "none", ApiFormat: "ollama"},
			},
			SecurityFilter: SecurityFilterConfig{Engine: EngineConfig{Provider: "ollama-test", Model: "test"}},
		}
		g := NewNenyaGateway(cfg, &SecretsConfig{}, slog.Default())

		_, err := g.callEngine(context.Background(), g.config.SecurityFilter.Engine, "sys", "input")
		if err == nil {
			t.Fatal("expected error for missing response field")
		}
	})
}

func TestBuildUpstreamRequest(t *testing.T) {
	secrets := &SecretsConfig{
		ProviderKeys: map[string]string{"gemini": "AIza..."},
	}
	cfg := Config{
		Providers: builtInProviders(),
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	headers := http.Header{}
	headers.Set("Authorization", "Bearer client-token")
	headers.Set("Content-Type", "application/json")
	headers.Set("Connection", "keep-alive")
	headers.Set("Transfer-Encoding", "chunked")

	req, err := g.buildUpstreamRequest(context.Background(), http.MethodPost, "https://example.com/api", []byte(`{}`), "gemini", headers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if req.Header.Get("Authorization") != "Bearer AIza..." {
		t.Errorf("expected provider API key, got %q", req.Header.Get("Authorization"))
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type preserved, got %q", req.Header.Get("Content-Type"))
	}
	if req.Header.Get("Connection") != "" {
		t.Errorf("hop-by-hop header Connection should be stripped, got %q", req.Header.Get("Connection"))
	}
	if req.Header.Get("Transfer-Encoding") != "" {
		t.Errorf("hop-by-hop header Transfer-Encoding should be stripped, got %q", req.Header.Get("Transfer-Encoding"))
	}
}

func TestForwardToUpstreamAllExhausted(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 1 << 20},
		Agents: map[string]AgentConfig{
			"my-agent": {
				Strategy:        "fallback",
				CooldownSeconds: 60,
				Models: []AgentModel{
					{Provider: "test", Model: "m1"},
					{Provider: "test", Model: "m2"},
				},
			},
		},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           upstream.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"my-agent","messages":[{"role":"user","content":"fail all"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when all targets exhausted, got %d: %s", w.Code, w.Body.String())
	}
	if callCount != 2 {
		t.Errorf("expected 2 upstream calls, got %d", callCount)
	}
}

func TestForwardToUpstreamQuotaCooldown(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"error":{"code":"RateLimitReached","message":"Rate limit of 50 per 86400s exceeded. Please wait 1400 seconds."}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"choices":[{"delta":{"content":"fallback ok"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 1 << 20},
		Agents: map[string]AgentConfig{
			"qa-agent": {
				Strategy:        "fallback",
				CooldownSeconds: 60,
				Models: []AgentModel{
					{Provider: "test", Model: "test-model-1"},
					{Provider: "test", Model: "test-model-2"},
				},
			},
		},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           upstream.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"qa-agent","messages":[{"role":"user","content":"test"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after quota fallback, got %d: %s", w.Code, w.Body.String())
	}
	if callCount != 2 {
		t.Errorf("expected 2 upstream calls, got %d", callCount)
	}

	coolKey := "qa-agent:test:test-model-1"
	g.agentMu.Lock()
	cd, exists := g.modelCooldowns[coolKey]
	g.agentMu.Unlock()
	if !exists {
		t.Fatal("expected cooldown to be set for quota-exhausted model")
	}
	expectedMin := 60 * time.Second
	if cd.Before(time.Now().Add(expectedMin)) {
		t.Errorf("expected cooldown >= %v, got %v (expires %v from now)", expectedMin, cd, time.Until(cd))
	}
}

func TestForwardToUpstreamNetworkError(t *testing.T) {
	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 1 << 20},
		Agents: map[string]AgentConfig{
			"my-agent": {
				Strategy:        "fallback",
				CooldownSeconds: 60,
				Models: []AgentModel{
					{Provider: "test", Model: "m1"},
					{Provider: "test", Model: "m2"},
				},
			},
		},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           "http://127.0.0.1:1/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"my-agent","messages":[{"role":"user","content":"network fail"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when all targets fail with network error, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleChatCompletionsAgentAllExcludedByMaxContext(t *testing.T) {
	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 1 << 20},
		Agents: map[string]AgentConfig{
			"my-agent": {
				Models: []AgentModel{
					{Provider: "test", Model: "m1", MaxContext: 10},
				},
			},
		},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           "http://127.0.0.1:1/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"my-agent","messages":[{"role":"user","content":"this is way more than 10 tokens worth of content"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 when all models excluded by max_context, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleChatCompletionsAgentNoModels(t *testing.T) {
	secrets := &SecretsConfig{ClientToken: "tok"}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 1 << 20},
		Agents: map[string]AgentConfig{
			"empty-agent": {},
		},
		Providers: builtInProviders(),
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"empty-agent","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when agent has no models, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleChatCompletionsPayloadTooLarge(t *testing.T) {
	secrets := &SecretsConfig{ClientToken: "tok"}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 100},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := strings.Repeat("x", 200)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized payload, got %d", w.Code)
	}
}

func TestHandleEmbeddingsSuccessful(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("expected /v1/embeddings, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data":   []map[string]interface{}{{"object": "embedding", "embedding": []float64{0.1, 0.2}}},
			"model":  "embed-test",
			"usage":  map[string]interface{}{"prompt_tokens": 5, "total_tokens": 5},
		})
	}))
	defer upstream.Close()

	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 1 << 20},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           upstream.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"embed-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"embed-test","input":"hello world"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleEmbeddingsNetworkError(t *testing.T) {
	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 1 << 20},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           "http://127.0.0.1:1/v1/chat/completions",
				RoutePrefixes: []string{"embed-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"embed-test","input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for network error, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleEmbeddingsPayloadTooLarge(t *testing.T) {
	secrets := &SecretsConfig{ClientToken: "tok"}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 100},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := strings.Repeat("x", 200)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
}

func TestHandleEmbeddingsInvalidJSON(t *testing.T) {
	secrets := &SecretsConfig{ClientToken: "tok"}
	g := NewNenyaGateway(Config{Server: ServerConfig{MaxBodyBytes: 1 << 20}}, secrets, slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`not json`))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d", w.Code)
	}
}

func TestHandleChatCompletionsPrefixCachePipeline(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("failed to decode prefix cache request: %v", err)
			return
		}
		msgs := body["messages"].([]interface{})
		firstRole := msgs[0].(map[string]interface{})["role"].(string)
		lastRole := msgs[len(msgs)-1].(map[string]interface{})["role"].(string)

		if firstRole != "system" {
			t.Errorf("expected first message role=system after pin, got %q", firstRole)
		}
		if lastRole != "user" {
			t.Errorf("expected last message role=user, got %q", lastRole)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"choices":[{"delta":{"content":"ok"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server:         ServerConfig{MaxBodyBytes: 1 << 20},
		Governance:     GovernanceConfig{ContextSoftLimit: 4000, ContextHardLimit: 24000},
		SecurityFilter: SecurityFilterConfig{Enabled: false},
		PrefixCache:    PrefixCacheConfig{Enabled: true, PinSystemFirst: true},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           upstream.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"test-small","messages":[{"role":"user","content":"hi"},{"role":"system","content":"be helpful"},{"role":"assistant","content":"ok"},{"role":"user","content":"bye"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleChatCompletionsWindowCompaction(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("failed to decode ollama request: %v", err)
			return
		}
		prompt, _ := body["prompt"].(string)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "Summarized: " + prompt[:min(50, len(prompt))],
		})
	}))
	defer ollama.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("failed to decode window compaction request: %v", err)
			return
		}
		msgs := body["messages"].([]interface{})
		firstRole := msgs[0].(map[string]interface{})["role"].(string)
		firstContent := msgs[0].(map[string]interface{})["content"].(string)

		if firstRole != "system" {
			t.Errorf("expected first message role=system after window compaction, got %q", firstRole)
		}
		if !strings.Contains(firstContent, "Nenya Window Summary") {
			t.Errorf("expected window summary marker, got: %s", firstContent[:min(100, len(firstContent))])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"choices":[{"delta":{"content":"ok"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}

	longContent := strings.Repeat("word ", 250)
	messages := `[{"role":"user","content":"` + longContent + `"},{"role":"assistant","content":"` + longContent + `"},{"role":"user","content":"` + longContent + `"},{"role":"assistant","content":"` + longContent + `"},{"role":"user","content":"` + longContent + `"},{"role":"assistant","content":"` + longContent + `"},{"role":"user","content":"` + longContent + `"},{"role":"assistant","content":"` + longContent + `"},{"role":"user","content":"` + longContent + `"},{"role":"user","content":"current question"}]`

	cfg := Config{
		Server:         ServerConfig{MaxBodyBytes: 10 << 20},
		Governance:     GovernanceConfig{ContextSoftLimit: 4000, ContextHardLimit: 24000},
		SecurityFilter: SecurityFilterConfig{Enabled: false},
		Window:         WindowConfig{Enabled: true, Mode: "summarize", ActiveMessages: 2, TriggerRatio: 0.5, MaxContext: 5000, SummaryMaxRunes: 2000, Engine: EngineConfig{Provider: "ollama", Model: "test", TimeoutSeconds: 30}},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           upstream.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"test-small","messages":` + messages + `}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSummarizeWithOllamaError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("failed to decode summarize request: %v", err)
			return
		}
		msgs := body["messages"].([]interface{})
		lastContent := msgs[len(msgs)-1].(map[string]interface{})["content"].(string)

		if !strings.Contains(lastContent, "Truncated") {
			t.Errorf("expected truncated content when Ollama fails, got: %s", lastContent[:min(100, len(lastContent))])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"choices":[{"delta":{"content":"ok"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	largeContent := strings.Repeat("x", 25000)
	cfg := Config{
		Server:         ServerConfig{MaxBodyBytes: 10 << 20},
		Governance:     GovernanceConfig{ContextSoftLimit: 4000, ContextHardLimit: 24000},
		SecurityFilter: SecurityFilterConfig{Enabled: false, Engine: EngineConfig{Provider: "ollama", Model: "test", TimeoutSeconds: 5}},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           upstream.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"test-small","messages":[{"role":"user","content":"` + largeContent + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (truncated fallback), got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthenticateRequest(t *testing.T) {
	tests := []struct {
		name   string
		auth   string
		token  string
		wantOK bool
		want   int
	}{
		{"valid bearer", "Bearer test-token", "test-token", true, 0},
		{"missing header", "", "test-token", false, 401},
		{"malformed no bearer", "test-token", "test-token", false, 401},
		{"wrong token", "Bearer wrong-token", "test-token", false, 403},
		{"bearer with spaces", "Bearer  test-token  ", "test-token", true, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewNenyaGateway(Config{}, &SecretsConfig{ClientToken: tt.token}, slog.Default())
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			w := httptest.NewRecorder()
			ok := g.authenticateRequest(req, w)
			if ok != tt.wantOK {
				t.Errorf("got ok=%v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK && w.Code != tt.want {
				t.Errorf("got %d, want %d", w.Code, tt.want)
			}
		})
	}
}

func TestHandleChatCompletionsWithUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"hello"}}]}` + "\n"))
		_, _ = w.Write([]byte(`data: {"choices":[{}]}` + "\n"))
		_, _ = w.Write([]byte(`data: {"usage":{"completion_tokens":10,"prompt_tokens":5,"total_tokens":15}}` + "\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 1 << 20},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           upstream.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"test-small","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestForwardToUpstreamNonRetryableEmptyBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer upstream.Close()

	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 1 << 20},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           upstream.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"test-small","messages":[{"role":"user","content":"fail"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
}

func TestForwardToUpstreamRateLimitSkip(t *testing.T) {
	upstream1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream1.Close()

	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"choices":[{"delta":{"content":"ok"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream2.Close()

	u1, _ := url.Parse(upstream1.URL)

	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server:     ServerConfig{MaxBodyBytes: 1 << 20},
		Governance: GovernanceConfig{RatelimitMaxRPM: 1},
		Agents: map[string]AgentConfig{
			"my-agent": {
				Strategy:        "fallback",
				CooldownSeconds: 60,
				Models: []AgentModel{
					{Provider: "test", Model: "m1", URL: upstream1.URL + "/v1/chat/completions"},
					{Provider: "test", Model: "m2", URL: upstream2.URL + "/v1/chat/completions"},
				},
			},
		},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           upstream1.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	g.rlMu.Lock()
	g.rateLimits[u1.Host] = &rateLimiter{
		rpmBucket:  0,
		tpmBucket:  100000,
		lastRefill: time.Now().Add(1 * time.Minute),
	}
	g.rlMu.Unlock()

	body := `{"model":"my-agent","messages":[{"role":"user","content":"rl skip"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (skipped first, succeeded on second), got %d: %s", w.Code, w.Body.String())
	}
}

func TestBuildUpstreamRequestInvalidURL(t *testing.T) {
	secrets := &SecretsConfig{ProviderKeys: map[string]string{"test": "key"}}
	cfg := Config{
		Providers: map[string]ProviderConfig{
			"test": {URL: "http://valid", RoutePrefixes: []string{"t"}, AuthStyle: "none"},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	req, err := g.buildUpstreamRequest(context.Background(), http.MethodPost, "://invalid-url", []byte(`{}`), "test", http.Header{})
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
	if req != nil {
		t.Error("expected nil request")
	}
}

func TestHandleChatCompletionsNoProviderNoDefault(t *testing.T) {
	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 1 << 20},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           "http://127.0.0.1:1/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"unknown-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown model with no provider, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleChatCompletionsMessagesNotArray(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"choices":[{"delta":{"content":"ok"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 1 << 20},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           upstream.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"test-small","messages":"not an array"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (skips pipeline), got %d: %s", w.Code, w.Body.String())
	}
}

func TestForwardToUpstreamBuildRequestError(t *testing.T) {
	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 1 << 20},
		Agents: map[string]AgentConfig{
			"my-agent": {
				Strategy:        "fallback",
				CooldownSeconds: 60,
				Models: []AgentModel{
					{Provider: "test", Model: "m1", URL: "://bad"},
					{Provider: "test", Model: "m2", URL: "://also-bad"},
				},
			},
		},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           "http://unused.com/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"my-agent","messages":[{"role":"user","content":"fail"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when all targets fail to build request, got %d: %s", w.Code, w.Body.String())
	}
}

func TestServeHTTPPanicRecovery(t *testing.T) {
	g := &NenyaGateway{
		config:    Config{},
		stats:     NewUsageTracker(),
		logger:    slog.Default(),
		providers: make(map[string]*Provider),
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("panic should have been recovered by ServeHTTP, got: %v", rec)
		}
	}()

	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK && w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 200 or 503, got %d", w.Code)
	}
}

func TestHandleChatCompletionsLastMessageNoTextContent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("failed to decode last message no text content request: %v", err)
			return
		}
		msgs := body["messages"].([]interface{})
		lastMsg := msgs[len(msgs)-1].(map[string]interface{})
		content := lastMsg["content"]
		if content == nil {
			t.Error("expected content to be preserved even when not text")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"choices":[{"delta":{"content":"ok"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server:         ServerConfig{MaxBodyBytes: 1 << 20},
		Governance:     GovernanceConfig{ContextSoftLimit: 4000, ContextHardLimit: 24000},
		SecurityFilter: SecurityFilterConfig{Enabled: false},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           upstream.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"test-small","messages":[{"role":"user","content":[{"type":"image_url","url":"http://example.com/img.png"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleEmbeddingsBuildRequestError(t *testing.T) {
	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 1 << 20},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           "http://example.com/v1/chat/completions",
				RoutePrefixes: []string{"embed-"},
				AuthStyle:     "unknown_style",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"embed-test","input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for build request error (unknown auth style), got %d: %s", w.Code, w.Body.String())
	}
}

func TestDetermineUpstreamNoProviders(t *testing.T) {
	cfg := Config{}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	result := g.determineUpstream("unknown")
	builtIn := builtInProviders()
	expected := builtIn["zai"].URL
	if result != expected {
		t.Errorf("expected default provider URL %q, got %q", expected, result)
	}
}

func TestHandleChatCompletionsWithEmptyMessages(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"choices":[{"delta":{"content":"ok"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"test": "api-key"},
	}
	cfg := Config{
		Server: ServerConfig{MaxBodyBytes: 1 << 20},
		Providers: map[string]ProviderConfig{
			"test": {
				URL:           upstream.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "bearer",
			},
		},
	}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	body := `{"model":"test-small","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestForwardToUpstreamClientDisconnect(t *testing.T) {
	slowUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slowUpstream.Close()

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g := NewNenyaGateway(Config{
			Server: ServerConfig{MaxBodyBytes: 1 << 20},
			Providers: map[string]ProviderConfig{
				"test": {
					URL:           slowUpstream.URL + "/v1/chat/completions",
					RoutePrefixes: []string{"test-"},
					AuthStyle:     "bearer",
				},
			},
		}, &SecretsConfig{
			ClientToken:  "tok",
			ProviderKeys: map[string]string{"test": "api-key"},
		}, slog.Default())
		g.ServeHTTP(w, r)
	}))
	defer gateway.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	body := `{"model":"test-small","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
}
