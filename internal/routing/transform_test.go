package routing

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"nenya/config"
	"nenya/internal/infra"
)

func testProviders() map[string]*config.Provider {
	cfg := &config.Config{}
	if err := config.ApplyDefaults(cfg); err != nil {
		panic(err)
	}
	return config.ResolveProviders(cfg, &config.SecretsConfig{
		ProviderKeys: map[string]string{
			"gemini":   "test-gemini-key",
			"deepseek": "test-ds-key",
			"zai":      "test-zai-key",
			"groq":     "test-groq-key",
		},
	})
}

func testDeps(providers map[string]*config.Provider) TransformDeps {
	return TransformDeps{
		Logger:          testLogger(),
		Providers:       providers,
		Config:          &config.Config{Agents: map[string]config.AgentConfig{}},
		ThoughtSigCache: nil,
		ExtractContentText: func(msg map[string]interface{}) string {
			if c, ok := msg["content"].(string); ok {
				return c
			}
			return ""
		},
		Catalog: nil,
	}
}

func TestTransformRequest_GeminiModelMapping(t *testing.T) {
	providers := testProviders()
	deps := testDeps(providers)

	tests := []struct {
		name     string
		model    string
		expected string
	}{
		{"gemini-3-flash to preview", "gemini-3-flash", "gemini-3-flash-preview"},
		{"gemini-3-pro to preview", "gemini-3-pro", "gemini-3-pro-preview"},
		{"gemini-3.1-flash to preview", "gemini-3.1-flash", "gemini-3.1-flash-preview"},
		{"gemini-3.1-flash-lite to preview", "gemini-3.1-flash-lite", "gemini-3.1-flash-lite-preview"},
		{"gemini-flash to 2.5", "gemini-flash", "gemini-2.5-flash"},
		{"gemini-flash-lite to 2.5-lite", "gemini-flash-lite", "gemini-2.5-flash-lite"},
		{"gemini-pro to 2.5-pro", "gemini-pro", "gemini-2.5-pro"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := map[string]interface{}{
				"model":    tt.model,
				"messages": []interface{}{},
			}
			body, returnedModel, err := TransformRequestForUpstream(deps, "gemini", "http://example.com", payload, "", 0, "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if returnedModel != tt.expected {
				t.Errorf("expected model %q, got %q", tt.expected, returnedModel)
			}
			var parsed map[string]interface{}
			if err := json.Unmarshal(body, &parsed); err != nil {
				t.Fatalf("failed to unmarshal body: %v", err)
			}
			if parsed["model"] != tt.expected {
				t.Errorf("expected body model %q, got %q", tt.expected, parsed["model"])
			}
			if payload["model"] != tt.model {
				t.Errorf("original payload mutated: got %v, want %v", payload["model"], tt.model)
			}
		})
	}
}

func TestTransformRequest_GeminiUnknownModel(t *testing.T) {
	providers := testProviders()
	deps := testDeps(providers)

	payload := map[string]interface{}{
		"model":    "gemini-totally-unknown",
		"messages": []interface{}{},
	}
	body, returnedModel, err := TransformRequestForUpstream(deps, "gemini", "http://example.com", payload, "", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if returnedModel != "gemini-totally-unknown" {
		t.Errorf("expected model %q, got %q", "gemini-totally-unknown", returnedModel)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("failed to unmarshal body: %v", err)
	}
	if parsed["model"] != "gemini-totally-unknown" {
		t.Errorf("expected body model %q, got %q", "gemini-totally-unknown", parsed["model"])
	}
}

func TestTransformRequest_NonGeminiNoMapping(t *testing.T) {
	providers := testProviders()
	deps := testDeps(providers)

	payload := map[string]interface{}{
		"model":    "deepseek-v4-flash",
		"messages": []interface{}{},
	}
	_, returnedModel, err := TransformRequestForUpstream(deps, "deepseek", "http://example.com", payload, "", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if returnedModel != "deepseek-v4-flash" {
		t.Errorf("expected model %q, got %q", "deepseek-v4-flash", returnedModel)
	}
	if payload["model"] != "deepseek-v4-flash" {
		t.Errorf("original payload mutated")
	}
}

func TestTransformRequest_AgentSystemPrompt(t *testing.T) {
	providers := testProviders()
	deps := testDeps(providers)
	deps.Config.Agents = map[string]config.AgentConfig{
		"my-agent": {
			SystemPrompt: "You are a helpful agent.",
		},
	}

	payload := map[string]interface{}{
		"model": "my-agent",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}
	body, _, err := TransformRequestForUpstream(deps, "deepseek", "http://example.com", payload, "deepseek-v4-flash", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("failed to unmarshal body: %v", err)
	}
	msgs, ok := parsed["messages"].([]interface{})
	if !ok {
		t.Fatal("messages not an array")
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	first, ok := msgs[0].(map[string]interface{})
	if !ok {
		t.Fatal("first message not a map")
	}
	if first["role"] != "system" || first["content"] != "You are a helpful agent." {
		t.Errorf("expected system prompt injection, got role=%q content=%q", first["role"], first["content"])
	}
}

func TestTransformRequest_AgentSystemPromptSkippedWhenSystemFirst(t *testing.T) {
	providers := testProviders()
	deps := testDeps(providers)
	deps.Config.Agents = map[string]config.AgentConfig{
		"my-agent": {
			SystemPrompt: "You are a helpful agent.",
		},
	}

	payload := map[string]interface{}{
		"model": "my-agent",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "Existing system"},
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}
	body, _, err := TransformRequestForUpstream(deps, "deepseek", "http://example.com", payload, "deepseek-v4-flash", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("failed to unmarshal body: %v", err)
	}
	msgs, ok := parsed["messages"].([]interface{})
	if !ok {
		t.Fatal("messages not an array")
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (no injection), got %d", len(msgs))
	}
	first, ok := msgs[0].(map[string]interface{})
	if !ok {
		t.Fatal("first message not a map")
	}
	if first["content"] != "Existing system" {
		t.Errorf("expected original system message, got %q", first["content"])
	}
}

func TestTransformRequest_ForceSystemPromptOverridesExistingSystem(t *testing.T) {
	providers := testProviders()
	deps := testDeps(providers)
	deps.Config.Agents = map[string]config.AgentConfig{
		"my-agent": {
			SystemPrompt:      "You are a helpful agent.",
			ForceSystemPrompt: true,
		},
	}

	payload := map[string]interface{}{
		"model": "my-agent",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "Existing system"},
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}
	body, _, err := TransformRequestForUpstream(deps, "deepseek", "http://example.com", payload, "deepseek-v4-flash", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("failed to unmarshal body: %v", err)
	}
	msgs, ok := parsed["messages"].([]interface{})
	if !ok {
		t.Fatal("messages not an array")
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (force injected + 2 original), got %d", len(msgs))
	}
	first, ok := msgs[0].(map[string]interface{})
	if !ok {
		t.Fatal("first message not a map")
	}
	if first["role"] != "system" || first["content"] != "You are a helpful agent." {
		t.Errorf("expected forced system prompt, got role=%q content=%q", first["role"], first["content"])
	}
	second, ok := msgs[1].(map[string]interface{})
	if !ok {
		t.Fatal("second message not a map")
	}
	if second["role"] != "system" || second["content"] != "Existing system" {
		t.Errorf("expected original system message preserved, got role=%q content=%q", second["role"], second["content"])
	}
}

func TestTransformRequest_NoModelField(t *testing.T) {
	providers := testProviders()
	deps := testDeps(providers)

	payload := map[string]interface{}{
		"messages": []interface{}{},
	}
	body, returnedModel, err := TransformRequestForUpstream(deps, "deepseek", "http://example.com", payload, "", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body != nil {
		t.Errorf("expected nil body, got %s", string(body))
	}
	if returnedModel != "" {
		t.Errorf("expected empty model, got %q", returnedModel)
	}
}

func TestTransformRequest_NonStringModel(t *testing.T) {
	providers := testProviders()
	deps := testDeps(providers)

	payload := map[string]interface{}{
		"model":    12345,
		"messages": []interface{}{},
	}
	body, returnedModel, err := TransformRequestForUpstream(deps, "deepseek", "http://example.com", payload, "", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body != nil {
		t.Errorf("expected nil body, got %s", string(body))
	}
	if returnedModel != "" {
		t.Errorf("expected empty model, got %q", returnedModel)
	}
}

func TestTransformRequest_ModelOverride(t *testing.T) {
	providers := testProviders()
	deps := testDeps(providers)

	payload := map[string]interface{}{
		"model":    "some-agent",
		"messages": []interface{}{},
	}
	_, returnedModel, err := TransformRequestForUpstream(deps, "deepseek", "http://example.com", payload, "deepseek-v4-flash", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if returnedModel != "deepseek-v4-flash" {
		t.Errorf("expected model %q, got %q", "deepseek-v4-flash", returnedModel)
	}
	if payload["model"] != "some-agent" {
		t.Errorf("original payload mutated: got %v", payload["model"])
	}
}

func TestTransformRequest_OriginalPayloadNotMutated(t *testing.T) {
	providers := testProviders()
	deps := testDeps(providers)

	origModel := "gemini-3-flash"
	payload := map[string]interface{}{
		"model":    origModel,
		"messages": []interface{}{},
	}
	_, _, err := TransformRequestForUpstream(deps, "gemini", "http://example.com", payload, "", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["model"] != origModel {
		t.Errorf("original payload model mutated: got %v, want %v", payload["model"], origModel)
	}
}

func TestTransformRequest_MaxTokensCapping(t *testing.T) {
	providers := testProviders()
	deps := testDeps(providers)

	t.Run("sets max_tokens from registry when absent", func(t *testing.T) {
		payload := map[string]interface{}{
			"model":    "gemini-2.5-flash",
			"messages": []interface{}{},
		}
		body, _, err := TransformRequestForUpstream(deps, "gemini", "http://example.com", payload, "", 0, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("failed to unmarshal body: %v", err)
		}
		mt, ok := parsed["max_tokens"]
		if !ok {
			t.Fatal("expected max_tokens to be set")
		}
		if mt.(float64) != 65536 {
			t.Errorf("expected max_tokens=65536, got %v", mt)
		}
	})

	t.Run("caps existing max_tokens when higher than registry", func(t *testing.T) {
		payload := map[string]interface{}{
			"model":      "gemini-2.5-flash",
			"messages":   []interface{}{},
			"max_tokens": float64(128000),
		}
		body, _, err := TransformRequestForUpstream(deps, "gemini", "http://example.com", payload, "", 0, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("failed to unmarshal body: %v", err)
		}
		if parsed["max_tokens"].(float64) != 65536 {
			t.Errorf("expected max_tokens capped to 65536, got %v", parsed["max_tokens"])
		}
	})

	t.Run("respects lower max_tokens when already below registry", func(t *testing.T) {
		payload := map[string]interface{}{
			"model":      "gemini-2.5-flash",
			"messages":   []interface{}{},
			"max_tokens": float64(4000),
		}
		body, _, err := TransformRequestForUpstream(deps, "gemini", "http://example.com", payload, "", 0, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("failed to unmarshal body: %v", err)
		}
		if parsed["max_tokens"].(float64) != 4000 {
			t.Errorf("expected max_tokens=4000, got %v", parsed["max_tokens"])
		}
	})

	t.Run("maxOutput param caps registry value", func(t *testing.T) {
		payload := map[string]interface{}{
			"model":    "gemini-2.5-flash",
			"messages": []interface{}{},
		}
		body, _, err := TransformRequestForUpstream(deps, "gemini", "http://example.com", payload, "", 4096, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("failed to unmarshal body: %v", err)
		}
		if parsed["max_tokens"].(float64) != 4096 {
			t.Errorf("expected max_tokens=4096, got %v", parsed["max_tokens"])
		}
	})
}

func TestInjectAPIKey_Bearer(t *testing.T) {
	providers := testProviders()
	headers := http.Header{}

	err := InjectAPIKey("deepseek", providers, headers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := headers.Get("Authorization"); got != "Bearer test-ds-key" {
		t.Errorf("expected 'Bearer test-ds-key', got %q", got)
	}
	if got := headers.Get("x-goog-api-key"); got != "" {
		t.Errorf("expected no x-goog-api-key header, got %q", got)
	}
}

func TestInjectAPIKey_BearerXGoog(t *testing.T) {
	providers := testProviders()
	headers := http.Header{}

	err := InjectAPIKey("gemini", providers, headers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := headers.Get("Authorization"); got != "Bearer test-gemini-key" {
		t.Errorf("expected 'Bearer test-gemini-key', got %q", got)
	}
	if got := headers.Get("x-goog-api-key"); got != "test-gemini-key" {
		t.Errorf("expected 'test-gemini-key', got %q", got)
	}
}

func TestInjectAPIKey_None(t *testing.T) {
	providers := testProviders()
	headers := http.Header{}

	err := InjectAPIKey("ollama", providers, headers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(headers) != 0 {
		t.Errorf("expected no headers, got %v", headers)
	}
}

func TestInjectAPIKey_UnknownProvider(t *testing.T) {
	providers := testProviders()
	headers := http.Header{}

	err := InjectAPIKey("nonexistent", providers, headers)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestInjectAPIKey_MissingAPIKey(t *testing.T) {
	providers := testProviders()
	delete(providers, "groq")
	providers["groq"] = &config.Provider{
		Name:      "groq",
		AuthStyle: "bearer",
		APIKey:    "",
	}
	headers := http.Header{}

	err := InjectAPIKey("groq", providers, headers)
	if err == nil {
		t.Fatal("expected error for missing API key")
	}
}

func TestCopyHeaders_Normal(t *testing.T) {
	src := http.Header{
		"Content-Type":  []string{"application/json"},
		"Authorization": []string{"Bearer token123"},
		"Accept":        []string{"application/json"},
	}
	dst := http.Header{}

	CopyHeaders(src, dst)

	if dst.Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type copied, got %q", dst.Get("Content-Type"))
	}
	if dst.Get("Authorization") != "Bearer token123" {
		t.Errorf("expected Authorization copied, got %q", dst.Get("Authorization"))
	}
	if dst.Get("Accept") != "application/json" {
		t.Errorf("expected Accept copied, got %q", dst.Get("Accept"))
	}
}

func TestCopyHeaders_HopByHopStripped(t *testing.T) {
	src := http.Header{
		"Connection":         []string{"keep-alive"},
		"Content-Length":     []string{"1234"},
		"Transfer-Encoding":  []string{"chunked"},
		"Content-Type":       []string{"application/json"},
		"Keep-Alive":         []string{"timeout=5"},
		"Te":                 []string{"trailers"},
		"Upgrade":            []string{"h2c"},
		"Proxy-Authorize":    []string{"Basic abc"},
		"Proxy-Authenticate": []string{"Basic realm=test"},
		"Trailers":           []string{"chunked"},
	}
	dst := http.Header{}

	CopyHeaders(src, dst)

	if dst.Get("Connection") != "" {
		t.Error("Connection should be stripped")
	}
	if dst.Get("Content-Length") != "" {
		t.Error("Content-Length should be stripped")
	}
	if dst.Get("Transfer-Encoding") != "" {
		t.Error("Transfer-Encoding should be stripped")
	}
	if dst.Get("Content-Type") != "application/json" {
		t.Error("Content-Type should be preserved")
	}
}

func TestCopyHeaders_EmptySource(t *testing.T) {
	src := http.Header{}
	dst := http.Header{"X-Existing": []string{"value"}}

	CopyHeaders(src, dst)

	if len(dst) != 1 {
		t.Errorf("expected dst unchanged with 1 header, got %d", len(dst))
	}
}

func TestSliceContains_Found(t *testing.T) {
	if !SliceContains([]int{1, 2, 3}, 2) {
		t.Error("expected true")
	}
}

func TestSliceContains_NotFound(t *testing.T) {
	if SliceContains([]int{1, 2, 3}, 5) {
		t.Error("expected false")
	}
}

func TestSliceContains_Empty(t *testing.T) {
	if SliceContains([]int{}, 1) {
		t.Error("expected false for empty slice")
	}
}

func TestTransformDeps_WithCache(t *testing.T) {
	cache := infra.NewThoughtSignatureCache(100, 10*time.Minute)
	cache.Store("tc-1", "cached-sig-value")

	providers := testProviders()
	deps := testDeps(providers)
	deps.ThoughtSigCache = cache

	payload := map[string]interface{}{
		"model": "gemini-3-flash",
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":   "tc-1",
						"type": "function",
						"function": map[string]interface{}{
							"name": "read_file",
						},
					},
				},
			},
			map[string]interface{}{
				"role":         "tool",
				"tool_call_id": "tc-1",
				"content":      "file contents here",
			},
		},
	}

	body, returnedModel, err := TransformRequestForUpstream(deps, "gemini", "http://example.com", payload, "", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if returnedModel != "gemini-3-flash-preview" {
		t.Errorf("expected mapped model, got %q", returnedModel)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	msgs := parsed["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	assistant := msgs[0].(map[string]interface{})
	tcs := assistant["tool_calls"].([]interface{})
	tc := tcs[0].(map[string]interface{})
	if tc["extra_content"] != "cached-sig-value" {
		t.Errorf("expected cached thought_signature injected, got %v", tc["extra_content"])
	}
}

func TestTransformRequest_ZAIWithTools_PreservesMessages(t *testing.T) {
	providers := testProviders()
	deps := testDeps(providers)

	payload := map[string]interface{}{
		"model": "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "first part"},
			map[string]interface{}{"role": "user", "content": "second part"},
			map[string]interface{}{"role": "assistant", "content": "response"},
		},
		"tools":       []interface{}{map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "read_file"}}},
		"tool_choice": "auto",
	}

	body, _, err := TransformRequestForUpstream(deps, "zai", "http://example.com", payload, "", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	msgs := parsed["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (untouched), got %d", len(msgs))
	}
	if msgs[0].(map[string]interface{})["role"] == "system" {
		t.Error("system bridge should NOT be prepended when tools are present")
	}
	if msgs[0].(map[string]interface{})["content"] != "first part" {
		t.Errorf("first message should be unchanged, got content=%q", msgs[0].(map[string]interface{})["content"])
	}
	if msgs[1].(map[string]interface{})["content"] != "second part" {
		t.Errorf("second message should be unchanged, got content=%q", msgs[1].(map[string]interface{})["content"])
	}
	if _, ok := parsed["tools"]; !ok {
		t.Error("tools should be preserved in output")
	}
	if parsed["tool_choice"] != "auto" {
		t.Errorf("tool_choice should be preserved, got %v", parsed["tool_choice"])
	}
}

func TestTransformRequest_ZAIWithTools_KeepsMaxTokens(t *testing.T) {
	providers := testProviders()
	deps := testDeps(providers)

	payload := map[string]interface{}{
		"model":      "glm-5-turbo",
		"messages":   []interface{}{map[string]interface{}{"role": "user", "content": "hello"}},
		"max_tokens": float64(16384),
		"tools":      []interface{}{},
	}

	body, _, err := TransformRequestForUpstream(deps, "zai", "http://example.com", payload, "", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if parsed["max_tokens"].(float64) != 16384 {
		t.Errorf("expected max_tokens=16384, got %v", parsed["max_tokens"])
	}
}

func TestTransformRequest_ZAIWithTools_KeepsStreamOptions(t *testing.T) {
	providers := testProviders()
	deps := testDeps(providers)

	payload := map[string]interface{}{
		"model":    "gpt-4o",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hello"}},
		"stream_options": map[string]interface{}{
			"include_usage": true,
		},
		"tools": []interface{}{},
	}

	body, _, err := TransformRequestForUpstream(deps, "zai", "http://example.com", payload, "", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if _, ok := parsed["stream_options"]; !ok {
		t.Error("stream_options should be kept for gpt-4o (inferred supports_stream_options)")
	}
}
