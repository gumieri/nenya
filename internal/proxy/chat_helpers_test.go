package proxy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nenya/config"
	"nenya/internal/gateway"
	"nenya/internal/infra"
	"nenya/internal/mcp"
	"nenya/internal/routing"
	"nenya/internal/testutil"
)

func TestHTTPError_Error(t *testing.T) {
	err := &httpError{Code: http.StatusBadRequest, Message: "bad request"}
	if got := err.Error(); got != "bad request" {
		t.Errorf("expected 'bad request', got %q", got)
	}
}

func TestGetAgentCooldown(t *testing.T) {
	tests := []struct {
		name     string
		seconds  int
		expected time.Duration
	}{
		{name: "custom cooldown", seconds: 30, expected: 30 * time.Second},
		{name: "zero uses default", seconds: 0, expected: time.Duration(routing.DefaultAgentCooldownSec) * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := config.AgentConfig{CooldownSeconds: tc.seconds}
			got := getAgentCooldown(agent)
			if got != tc.expected {
				t.Errorf("expected %v, got %v", tc.expected, got)
			}
		})
	}
}

func TestHandleEmptyAgentTargets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	gw := p.Gateway()

	t.Run("agent has models but all excluded", func(t *testing.T) {
		req := &chatRequest{
			ModelName:  "test-agent",
			TokenCount: 999999,
		}
		agent := config.AgentConfig{
			Models: []config.AgentModel{
				{Provider: "test-provider", Model: "test-model", MaxContext: 100},
			},
		}
		_, _, _, _, herr := handleEmptyAgentTargets(req, gw, agent)
		if herr == nil {
			t.Fatal("expected httpError, got nil")
		}
		if herr.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("expected 413, got %d", herr.Code)
		}
	})

	t.Run("agent has no models", func(t *testing.T) {
		req := &chatRequest{ModelName: "empty-agent"}
		agent := config.AgentConfig{Models: []config.AgentModel{}}
		_, _, _, _, herr := handleEmptyAgentTargets(req, gw, agent)
		if herr == nil {
			t.Fatal("expected httpError, got nil")
		}
		if herr.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", herr.Code)
		}
	})
}

func TestCountEmbeddingInputTokens(t *testing.T) {
	tests := []struct {
		name     string
		payload  map[string]interface{}
		expected int
	}{
		{name: "no input", payload: map[string]interface{}{}, expected: 0},
		{name: "string input", payload: map[string]interface{}{"input": "hello world"}, expected: len("hello world") / 4},
		{name: "string short", payload: map[string]interface{}{"input": "ab"}, expected: 1},
		{name: "array of strings", payload: map[string]interface{}{"input": []interface{}{"hello", "world"}}, expected: len("hello world") / 4},
		{name: "empty array", payload: map[string]interface{}{"input": []interface{}{}}, expected: 0},
		{name: "wrong type", payload: map[string]interface{}{"input": 42}, expected: 0},
		{name: "mixed array", payload: map[string]interface{}{"input": []interface{}{"hello", 42}}, expected: len("hello ") / 4},
		{name: "empty string", payload: map[string]interface{}{"input": ""}, expected: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := countEmbeddingInputTokens(tc.payload)
			if got != tc.expected {
				t.Errorf("expected %d, got %d", tc.expected, got)
			}
		})
	}
}

func TestIsPathSafeResponses(t *testing.T) {
	tests := []struct {
		name string
		path string
		safe bool
	}{
		{name: "root", path: "/v1/responses", safe: true},
		{name: "by ID", path: "/v1/responses/resp_123", safe: true},
		{name: "cancel", path: "/v1/responses/resp_456/cancel", safe: true},
		{name: "path traversal", path: "/v1/responses/../../etc/passwd", safe: false},
		{name: "wrong prefix", path: "/v1/chat/completions", safe: false},
		{name: "empty", path: "", safe: false},
		{name: "deep path", path: "/v1/responses/a/b/c", safe: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{}
			got := p.isPathSafeResponses(tc.path)
			if got != tc.safe {
				t.Errorf("isPathSafeResponses(%q) = %v, want %v", tc.path, got, tc.safe)
			}
		})
	}
}

func TestDetectRequestCapabilities(t *testing.T) {
	t.Run("no tools, no content arrays", func(t *testing.T) {
		caps := detectRequestCapabilities(map[string]interface{}{
			"messages": []interface{}{},
		})
		if caps.HasToolCalls || caps.HasContentArr || caps.HasVision || caps.HasReasoning {
			t.Errorf("expected all false, got %+v", caps)
		}
	})

	t.Run("has tools", func(t *testing.T) {
		caps := detectRequestCapabilities(map[string]interface{}{
			"tools": []interface{}{"tool1"},
		})
		if !caps.HasToolCalls {
			t.Errorf("expected HasToolCalls=true")
		}
	})

	t.Run("no messages key", func(t *testing.T) {
		caps := detectRequestCapabilities(map[string]interface{}{
			"model": "test",
		})
		if caps.HasToolCalls {
			t.Errorf("expected no tool calls")
		}
	})

	t.Run("content array with vision", func(t *testing.T) {
		caps := detectRequestCapabilities(map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{
					"role": "user",
					"content": []interface{}{
						map[string]interface{}{"type": "text", "text": "hello"},
						map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64,abc"}},
					},
				},
			},
		})
		if !caps.HasContentArr {
			t.Errorf("expected HasContentArr=true")
		}
		if !caps.HasVision {
			t.Errorf("expected HasVision=true")
		}
	})

	t.Run("reasoning field", func(t *testing.T) {
		caps := detectRequestCapabilities(map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{
					"role":      "assistant",
					"content":   "thinking...",
					"reasoning": map[string]interface{}{"type": "enabled"},
				},
			},
		})
		if !caps.HasReasoning {
			t.Errorf("expected HasReasoning=true")
		}
	})

	t.Run("non-map messages skipped", func(t *testing.T) {
		caps := detectRequestCapabilities(map[string]interface{}{
			"messages": []interface{}{"not a map"},
		})
		if caps.HasContentArr || caps.HasVision || caps.HasReasoning {
			t.Errorf("expected all false for non-map messages")
		}
	})
}

func TestInspectMessageCaps(t *testing.T) {
	t.Run("short-circuits on vision+reasoning", func(t *testing.T) {
		caps := &routing.RequestCapabilities{}
		msg := map[string]interface{}{
			"content": []interface{}{
				map[string]interface{}{"type": "image_url"},
			},
			"reasoning": map[string]interface{}{"key": "val"},
		}
		done := inspectMessageCaps(msg, caps)
		if !done {
			t.Errorf("expected early return true")
		}
	})

	t.Run("array content but no vision", func(t *testing.T) {
		caps := &routing.RequestCapabilities{}
		msg := map[string]interface{}{
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "hello"},
			},
		}
		done := inspectMessageCaps(msg, caps)
		if done {
			t.Errorf("expected no early return")
		}
		if !caps.HasContentArr {
			t.Errorf("expected HasContentArr=true")
		}
	})

	t.Run("non-map message returns false", func(t *testing.T) {
		caps := &routing.RequestCapabilities{}
		done := inspectMessageCaps("not a map", caps)
		if done {
			t.Errorf("expected false for non-map")
		}
	})
}

func TestApplyRedactToContent(t *testing.T) {
	noopRedact := func(s string) string { return s }
	redactAll := func(s string) string { return "[REDACTED]" }

	t.Run("no content key", func(t *testing.T) {
		changed := applyRedactToContent(map[string]interface{}{}, noopRedact)
		if changed {
			t.Errorf("expected false for no content")
		}
	})

	t.Run("empty string content", func(t *testing.T) {
		changed := applyRedactToContent(map[string]interface{}{"content": ""}, noopRedact)
		if changed {
			t.Errorf("expected false for empty content")
		}
	})

	t.Run("string content unchanged", func(t *testing.T) {
		node := map[string]interface{}{"content": "hello"}
		changed := applyRedactToContent(node, noopRedact)
		if changed {
			t.Errorf("expected false when nothing changed")
		}
	})

	t.Run("string content redacted", func(t *testing.T) {
		node := map[string]interface{}{"content": "my password is secret"}
		changed := applyRedactToContent(node, redactAll)
		if !changed {
			t.Errorf("expected true when redacted")
		}
		if node["content"] != "[REDACTED]" {
			t.Errorf("expected [REDACTED], got %v", node["content"])
		}
	})

	t.Run("array content with text parts", func(t *testing.T) {
		node := map[string]interface{}{
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "hello"},
				map[string]interface{}{"type": "image_url", "url": "http://example.com/img.png"},
			},
		}
		changed := applyRedactToContent(node, redactAll)
		if !changed {
			t.Errorf("expected true when text part redacted")
		}
		parts := node["content"].([]interface{})
		first := parts[0].(map[string]interface{})
		if first["text"] != "[REDACTED]" {
			t.Errorf("expected text part redacted, got %v", first["text"])
		}
		second := parts[1].(map[string]interface{})
		if second["url"] != "http://example.com/img.png" {
			t.Errorf("expected non-text part unchanged, got %v", second["url"])
		}
	})

	t.Run("array content non-map items", func(t *testing.T) {
		node := map[string]interface{}{
			"content": []interface{}{"not a map"},
		}
		changed := applyRedactToContent(node, redactAll)
		if changed {
			t.Errorf("expected false for non-map items")
		}
	})

	t.Run("array content missing text key", func(t *testing.T) {
		node := map[string]interface{}{
			"content": []interface{}{
				map[string]interface{}{"type": "text"},
			},
		}
		changed := applyRedactToContent(node, redactAll)
		if changed {
			t.Errorf("expected false for missing text key")
		}
	})

	t.Run("non-string non-array content", func(t *testing.T) {
		node := map[string]interface{}{"content": 42}
		changed := applyRedactToContent(node, redactAll)
		if changed {
			t.Errorf("expected false for non-string non-array content")
		}
	})
}

func TestBuildProviderTargets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	gw := p.Gateway()

	t.Run("builds targets from matches", func(t *testing.T) {
		matches := []routing.ProviderMatch{
			{Provider: "test-provider", Model: "test-model", MaxContext: 8000, MaxOutput: 2048},
		}
		targets := buildProviderTargets(matches, gw)
		if len(targets) != 1 {
			t.Fatalf("expected 1 target, got %d", len(targets))
		}
		if targets[0].Model != "test-model" {
			t.Errorf("expected test-model, got %s", targets[0].Model)
		}
		if targets[0].MaxContext != 8000 {
			t.Errorf("expected 8000, got %d", targets[0].MaxContext)
		}
	})

	t.Run("skips unknown provider", func(t *testing.T) {
		matches := []routing.ProviderMatch{
			{Provider: "nonexistent", Model: "test-model"},
		}
		targets := buildProviderTargets(matches, gw)
		if len(targets) != 0 {
			t.Errorf("expected 0 targets for unknown provider, got %d", len(targets))
		}
	})

	t.Run("empty matches", func(t *testing.T) {
		targets := buildProviderTargets(nil, gw)
		if len(targets) != 0 {
			t.Errorf("expected 0 targets for nil matches, got %d", len(targets))
		}
	})
}

func TestRecordChatUsage(t *testing.T) {
	t.Run("records usage from total_tokens", func(t *testing.T) {
		gw := &gateway.NenyaGateway{Stats: infra.NewUsageTracker()}
		recordChatUsage(gw, "test-model", map[string]interface{}{
			"total_tokens":  100.0,
			"prompt_tokens": 40.0,
		})
	})

	t.Run("no total_tokens", func(t *testing.T) {
		gw := &gateway.NenyaGateway{Stats: infra.NewUsageTracker()}
		recordChatUsage(gw, "test-model", map[string]interface{}{
			"prompt_tokens": 40.0,
		})
	})

	t.Run("empty usage map", func(t *testing.T) {
		gw := &gateway.NenyaGateway{Stats: infra.NewUsageTracker()}
		recordChatUsage(gw, "test-model", map[string]interface{}{})
	})

	t.Run("nil stats - no panic", func(t *testing.T) {
		gw := &gateway.NenyaGateway{Stats: nil}
		recordChatUsage(gw, "test-model", map[string]interface{}{
			"total_tokens": 100.0,
		})
	})
}

func TestRecordUsageFromMap(t *testing.T) {
	t.Run("records with stats and metrics", func(t *testing.T) {
		gw := &gateway.NenyaGateway{
			Stats:   infra.NewUsageTracker(),
			Metrics: infra.NewMetrics(),
		}
		recordUsageFromMap(gw, map[string]interface{}{
			"usage": map[string]interface{}{
				"total_tokens":  200.0,
				"prompt_tokens": 80.0,
			},
		}, "test-model", "test-provider")
	})

	t.Run("no usage key", func(t *testing.T) {
		gw := &gateway.NenyaGateway{Stats: infra.NewUsageTracker()}
		recordUsageFromMap(gw, map[string]interface{}{"id": "123"}, "test-model", "test-provider")
	})

	t.Run("nil stats", func(t *testing.T) {
		gw := &gateway.NenyaGateway{Stats: nil}
		recordUsageFromMap(gw, map[string]interface{}{
			"usage": map[string]interface{}{"total_tokens": 200.0},
		}, "test-model", "")
	})

	t.Run("nil metrics", func(t *testing.T) {
		gw := &gateway.NenyaGateway{Stats: infra.NewUsageTracker(), Metrics: nil}
		recordUsageFromMap(gw, map[string]interface{}{
			"usage": map[string]interface{}{
				"total_tokens":  200.0,
				"prompt_tokens": 80.0,
			},
		}, "test-model", "test-provider")
	})
}

func TestGetDefaultResponseProvider(t *testing.T) {
	p := &Proxy{}

	t.Run("prefers deepseek", func(t *testing.T) {
		gw := &gateway.NenyaGateway{
			Providers: map[string]*config.Provider{
				"deepseek": {Name: "deepseek", APIKey: "sk-ds", AuthStyle: "bearer"},
			},
		}
		pr := p.getDefaultResponseProvider(gw)
		if pr == nil || pr.Name != "deepseek" {
			t.Errorf("expected deepseek, got %v", pr)
		}
	})

	t.Run("falls back to any provider with API key", func(t *testing.T) {
		gw := &gateway.NenyaGateway{
			Providers: map[string]*config.Provider{
				"custom": {Name: "custom", APIKey: "sk-custom", AuthStyle: "bearer"},
			},
		}
		pr := p.getDefaultResponseProvider(gw)
		if pr == nil || pr.Name != "custom" {
			t.Errorf("expected custom, got %v", pr)
		}
	})

	t.Run("falls back to no-auth provider", func(t *testing.T) {
		gw := &gateway.NenyaGateway{
			Providers: map[string]*config.Provider{
				"local": {Name: "local", APIKey: "", AuthStyle: "none"},
			},
		}
		pr := p.getDefaultResponseProvider(gw)
		if pr == nil || pr.Name != "local" {
			t.Errorf("expected local, got %v", pr)
		}
	})

	t.Run("no providers available", func(t *testing.T) {
		gw := &gateway.NenyaGateway{Providers: map[string]*config.Provider{}}
		pr := p.getDefaultResponseProvider(gw)
		if pr != nil {
			t.Errorf("expected nil, got %v", pr)
		}
	})
}

func TestResolveResponsesURL(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		provURL  string
		path     string
		query    string
		expected string
	}{
		{
			name: "base URL with /v1 suffix",
			baseURL: "https://api.example.com/v1",
			provURL: "https://api.example.com/v1/chat/completions",
			path: "/v1/responses/resp_123",
			expected: "https://api.example.com/v1/responses/resp_123",
		},
		{
			name: "base URL without /v1 suffix",
			baseURL: "https://api.example.com",
			provURL: "https://api.example.com/v1/chat/completions",
			path: "/v1/responses/resp_123",
			expected: "https://api.example.com/v1/responses/resp_123",
		},
		{
			name: "with query string",
			baseURL: "https://api.example.com/v1",
			provURL: "https://api.example.com/v1/chat/completions",
			path: "/v1/responses",
			query: "limit=10",
			expected: "https://api.example.com/v1/responses?limit=10",
		},
		{
			name: "subpath on responses",
			baseURL: "https://api.example.com/v1",
			provURL: "https://api.example.com/v1/chat/completions",
			path: "/v1/responses/resp_456/cancel",
			expected: "https://api.example.com/v1/responses/resp_456/cancel",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{}
			provider := &config.Provider{BaseURL: tc.baseURL, URL: tc.provURL}
			got := p.resolveResponsesURL(provider, tc.path, tc.query)
			if got != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, got)
			}
		})
	}

	t.Run("empty base URL derives from provider URL", func(t *testing.T) {
		p := &Proxy{}
		provider := &config.Provider{
			BaseURL: "",
			URL:     "https://api.example.com/v1/chat/completions",
		}
		got := p.resolveResponsesURL(provider, "/v1/responses/resp_123", "")
		expected := "https://api.example.com/v1/responses/resp_123"
		if got != expected {
			t.Errorf("expected %q, got %q", expected, got)
		}
	})

	t.Run("same as provider URL returns empty", func(t *testing.T) {
		p := &Proxy{}
		provider := &config.Provider{
			BaseURL: "https://api.other.com",
			URL:     "https://api.other.com",
		}
		got := p.resolveResponsesURL(provider, "/v1/responses/resp_123", "")
		if got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})
}

func TestPrepareOriginalPayload(t *testing.T) {
	t.Run("no minify", func(t *testing.T) {
		cfg := testutil.MinimalConfig()
		cfg.Compaction.Enabled = config.PtrTo(false)
		gw := &gateway.NenyaGateway{Config: *cfg}
		payload := map[string]interface{}{"key": "value"}
		data, err := prepareOriginalPayload(gw, payload)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}
		if decoded["key"] != "value" {
			t.Errorf("expected value, got %v", decoded["key"])
		}
	})

	t.Run("with minify", func(t *testing.T) {
		cfg := testutil.MinimalConfig()
		cfg.Compaction.Enabled = config.PtrTo(true)
		cfg.Compaction.JSONMinify = config.PtrTo(true)
		gw := &gateway.NenyaGateway{Config: *cfg}
		payload := map[string]interface{}{
			"model": "test",
			"messages": []interface{}{
				map[string]interface{}{"role": "user", "content": "hi"},
			},
		}
		data, err := prepareOriginalPayload(gw, payload)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}
		if decoded["model"] != "test" {
			t.Errorf("expected test, got %v", decoded["model"])
		}
	})

	t.Run("minify enabled produces valid JSON", func(t *testing.T) {
		cfg := testutil.MinimalConfig()
		cfg.Compaction.Enabled = config.PtrTo(true)
		cfg.Compaction.JSONMinify = config.PtrTo(true)
		gw := &gateway.NenyaGateway{Config: *cfg}
		payload := map[string]interface{}{"key": "value"}
		data, err := prepareOriginalPayload(gw, payload)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(data) == 0 {
			t.Fatal("expected non-empty data")
		}
	})
}

func TestExtractAutoSearchQuery(t *testing.T) {
	p := &Proxy{}

	t.Run("last message is user", func(t *testing.T) {
		messages := []interface{}{
			map[string]interface{}{"role": "user", "content": "hello world"},
		}
		query, lastMsg := p.extractAutoSearchQuery(messages)
		if query != "hello world" {
			t.Errorf("expected 'hello world', got %q", query)
		}
		if lastMsg == nil {
			t.Errorf("expected lastMsg, got nil")
		}
	})

	t.Run("last message is not user", func(t *testing.T) {
		messages := []interface{}{
			map[string]interface{}{"role": "assistant", "content": "hello"},
		}
		query, lastMsg := p.extractAutoSearchQuery(messages)
		if query != "" {
			t.Errorf("expected empty query, got %q", query)
		}
		if lastMsg != nil {
			t.Errorf("expected nil lastMsg, got %v", lastMsg)
		}
	})

	t.Run("empty messages", func(t *testing.T) {
		query, lastMsg := p.extractAutoSearchQuery(nil)
		if query != "" {
			t.Errorf("expected empty query, got %q", query)
		}
		if lastMsg != nil {
			t.Errorf("expected nil lastMsg, got %v", lastMsg)
		}
	})

	t.Run("last message not a map", func(t *testing.T) {
		messages := []interface{}{"not a map"}
		query, lastMsg := p.extractAutoSearchQuery(messages)
		if query != "" {
			t.Errorf("expected empty query")
		}
		if lastMsg != nil {
			t.Errorf("expected nil")
		}
	})

	t.Run("last message missing role key", func(t *testing.T) {
		messages := []interface{}{
			map[string]interface{}{"content": "hello"},
		}
		query, _ := p.extractAutoSearchQuery(messages)
		if query != "" {
			t.Errorf("expected empty query for missing role")
		}
	})

	t.Run("content array in last message", func(t *testing.T) {
		messages := []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "hello from array"},
				},
			},
		}
		query, _ := p.extractAutoSearchQuery(messages)
		if query != "hello from array" {
			t.Errorf("expected 'hello from array', got %q", query)
		}
	})
}

func TestBuildResponsesContext(t *testing.T) {
	p := &Proxy{}

	t.Run("with timeout", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		provider := &config.Provider{TimeoutSeconds: 5}
		ctx, cancel := p.buildResponsesContext(req, provider)
		defer cancel()
		if ctx == nil {
			t.Fatal("expected non-nil context")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Errorf("expected deadline")
		}
	})

	t.Run("no timeout", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		provider := &config.Provider{TimeoutSeconds: 0}
		ctx, cancel := p.buildResponsesContext(req, provider)
		defer cancel()
		if ctx == nil {
			t.Fatal("expected non-nil context")
		}
	})
}

func TestReadResponsesBody(t *testing.T) {
	p := &Proxy{}

	t.Run("GET request returns empty", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/responses/resp_123", nil)
		cfg := testutil.MinimalConfig()
		gw := &gateway.NenyaGateway{Config: *cfg, Logger: slog.Default()}
		w := httptest.NewRecorder()
		body, ok := p.readResponsesBody(gw, w, req)
		if !ok {
			t.Errorf("expected ok=true")
		}
		if len(body) != 0 {
			t.Errorf("expected empty body, got %d bytes", len(body))
		}
	})

	t.Run("DELETE request returns empty", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/v1/responses/resp_123", nil)
		cfg := testutil.MinimalConfig()
		gw := &gateway.NenyaGateway{Config: *cfg, Logger: slog.Default()}
		w := httptest.NewRecorder()
		body, ok := p.readResponsesBody(gw, w, req)
		if !ok {
			t.Errorf("expected ok=true")
		}
		if len(body) != 0 {
			t.Errorf("expected empty body")
		}
	})

	t.Run("POST reads body", func(t *testing.T) {
		bodyStr := `{"model":"test"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(bodyStr))
		cfg := testutil.MinimalConfig()
		gw := &gateway.NenyaGateway{Config: *cfg, Logger: slog.Default()}
		w := httptest.NewRecorder()
		body, ok := p.readResponsesBody(gw, w, req)
		if !ok {
			t.Errorf("expected ok=true")
		}
		if string(body) != bodyStr {
			t.Errorf("expected %q, got %q", bodyStr, string(body))
		}
	})

	t.Run("POST body too large", func(t *testing.T) {
		cfg := testutil.MinimalConfig()
		cfg.Server.MaxBodyBytes = 10
		gw := &gateway.NenyaGateway{Config: *cfg, Logger: slog.Default()}
		bodyReader := strings.NewReader(`{"model":"test with very long content that exceeds the limit"}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", bodyReader)
		w := httptest.NewRecorder()
		_, ok := p.readResponsesBody(gw, w, req)
		if ok {
			t.Errorf("expected ok=false for too large body")
		}
	})
}

func TestHasMCPTools(t *testing.T) {
	p := &Proxy{}

	t.Run("empty agent name", func(t *testing.T) {
		gw := &gateway.NenyaGateway{Config: config.Config{Agents: nil}}
		got := p.hasMCPTools(gw, "")
		if got {
			t.Errorf("expected false for empty agent name")
		}
	})

	t.Run("agent not found", func(t *testing.T) {
		gw := &gateway.NenyaGateway{
			Config: config.Config{
				Agents: map[string]config.AgentConfig{},
			},
		}
		got := p.hasMCPTools(gw, "nonexistent")
		if got {
			t.Errorf("expected false for unknown agent")
		}
	})
}

func TestResolveSearchTool(t *testing.T) {
	p := &Proxy{}

	t.Run("uses configured tool name", func(t *testing.T) {
		gw := &gateway.NenyaGateway{
			MCPClients: map[string]*mcp.Client{},
		}
		tool := p.resolveSearchTool(gw, "test-server", "my-search-tool", "test-agent")
		if tool != "my-search-tool" {
			t.Errorf("expected 'my-search-tool', got %q", tool)
		}
	})

	t.Run("empty configured tool and server with empty tools", func(t *testing.T) {
		ms := newTestMCPServer(t)
		gw := &gateway.NenyaGateway{
			MCPClients: ms.clients(),
			Logger:     newTestLogger(),
		}
		tool := p.resolveSearchTool(gw, ms.serverName(), "", "test-agent")
		if tool != "" {
			t.Errorf("expected empty string, got %q", tool)
		}
	})
}

func TestCanPerformAutoSearch(t *testing.T) {
	p := &Proxy{}

	t.Run("server not found", func(t *testing.T) {
		gw := &gateway.NenyaGateway{MCPClients: map[string]*mcp.Client{}}
		if p.canPerformAutoSearch(gw, "nonexistent") {
			t.Errorf("expected false")
		}
	})

	t.Run("server exists but not ready", func(t *testing.T) {
		ms := newTestMCPServer(t)
		gw := &gateway.NenyaGateway{
			MCPClients: ms.clients(),
			Logger:     newTestLogger(),
		}
		if p.canPerformAutoSearch(gw, ms.serverName()) {
			t.Errorf("expected false for non-ready server")
		}
	})
}

func TestDiscoverToolByPrefix(t *testing.T) {
	p := &Proxy{}

	t.Run("server not found", func(t *testing.T) {
		gw := &gateway.NenyaGateway{MCPClients: map[string]*mcp.Client{}}
		tool := p.discoverToolByPrefix(gw, "nonexistent", "search")
		if tool != "" {
			t.Errorf("expected empty, got %q", tool)
		}
	})

	t.Run("server exists but no matching tool", func(t *testing.T) {
		ms := newTestMCPServer(t)
		gw := &gateway.NenyaGateway{
			MCPClients: ms.clients(),
			Logger:     newTestLogger(),
		}
		tool := p.discoverToolByPrefix(gw, ms.serverName(), "nonexistent")
		if tool != "" {
			t.Errorf("expected empty, got %q", tool)
		}
	})
}
