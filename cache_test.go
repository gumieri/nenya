package main

import (
	"log/slog"
	"net/http"
	"testing"
	"time"
)

func TestThoughtSignatureCache_StoreAndLoad(t *testing.T) {
	cache := NewThoughtSignatureCache(100, 5*time.Minute)

	cache.Store("tc-1", map[string]interface{}{"google": map[string]string{"thought_signature": "abc"}})
	cache.Store("tc-2", map[string]interface{}{"google": map[string]string{"thought_signature": "def"}})

	val, ok := cache.Load("tc-1")
	if !ok {
		t.Fatal("expected tc-1 to be found")
	}
	sig, _ := val.(map[string]interface{})
	google, _ := sig["google"].(map[string]string)
	if google["thought_signature"] != "abc" {
		t.Errorf("expected abc, got %s", google["thought_signature"])
	}

	val, ok = cache.Load("tc-2")
	if !ok {
		t.Fatal("expected tc-2 to be found")
	}

	_, ok = cache.Load("tc-nonexistent")
	if ok {
		t.Error("expected nonexistent key to miss")
	}
}

func TestThoughtSignatureCache_Expiration(t *testing.T) {
	cache := NewThoughtSignatureCache(100, 50*time.Millisecond)

	cache.Store("tc-expire", "value")

	_, ok := cache.Load("tc-expire")
	if !ok {
		t.Fatal("expected tc-expire to be found immediately")
	}

	time.Sleep(100 * time.Millisecond)

	_, ok = cache.Load("tc-expire")
	if ok {
		t.Error("expected tc-expire to be expired after TTL")
	}
}

func TestThoughtSignatureCache_Eviction(t *testing.T) {
	cache := NewThoughtSignatureCache(5, 1*time.Minute)

	for i := 0; i < 5; i++ {
		cache.Store(string(rune('a'+i)), i)
	}

	if cache.Len() != 5 {
		t.Fatalf("expected 5 entries, got %d", cache.Len())
	}

	cache.Store("f", "overflow")

	if cache.Len() > 5 {
		t.Errorf("expected at most 5 entries after overflow, got %d", cache.Len())
	}
}

func TestThoughtSignatureCache_NilAndEmptyKey(t *testing.T) {
	cache := NewThoughtSignatureCache(100, 1*time.Minute)

	cache.Store("", "ignored")
	cache.Store("key", nil)

	if cache.Len() != 0 {
		t.Errorf("expected 0 entries, got %d", cache.Len())
	}
}

func TestThoughtSignatureCache_Len(t *testing.T) {
	cache := NewThoughtSignatureCache(100, 1*time.Minute)

	if cache.Len() != 0 {
		t.Errorf("expected 0, got %d", cache.Len())
	}

	cache.Store("a", "1")
	cache.Store("b", "2")

	if cache.Len() != 2 {
		t.Errorf("expected 2, got %d", cache.Len())
	}
}

func TestGeminiTransformer_ExtractsExtraContent(t *testing.T) {
	var capturedID string
	var capturedExtra interface{}

	transformer := &GeminiTransformer{
		onExtraContent: func(toolCallID string, extraContent interface{}) {
			capturedID = toolCallID
			capturedExtra = extraContent
		},
	}

	input := `{"choices":[{"delta":{"tool_calls":[{"id":"tc-42","type":"function","function":{"name":"read"},"extra_content":{"google":{"thought_signature":"sig123"}}}]}}]}`

	_, err := transformer.TransformSSEChunk([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedID != "tc-42" {
		t.Errorf("expected tool_call_id tc-42, got %q", capturedID)
	}

	extraMap, ok := capturedExtra.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map, got %T", capturedExtra)
	}
	google, ok := extraMap["google"].(map[string]interface{})
	if !ok {
		t.Fatal("expected google key")
	}
	if google["thought_signature"] != "sig123" {
		t.Errorf("expected sig123, got %v", google["thought_signature"])
	}
}

func TestGeminiTransformer_NoCallbackNoPanic(t *testing.T) {
	transformer := &GeminiTransformer{}

	input := `{"choices":[{"delta":{"tool_calls":[{"id":"tc-1","type":"function","function":{"name":"read"},"extra_content":{"google":{"thought_signature":"sig"}}}]}}]}`

	_, err := transformer.TransformSSEChunk([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseRetryDelay(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		body   string
		want   time.Duration
	}{
		{
			name:   "no header no body",
			header: http.Header{},
			body:   "",
			want:   0,
		},
		{
			name:   "retry-after header",
			header: http.Header{"Retry-After": []string{"2"}},
			body:   "",
			want:   2 * time.Second,
		},
		{
			name:   "retry-after header capped",
			header: http.Header{"Retry-After": []string{"60"}},
			body:   "",
			want:   maxRetryBackoff,
		},
		{
			name:   "gemini retry delay in body",
			header: http.Header{},
			body:   `[{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"1.854618161s"}]}}]`,
			want:   1854618161 * time.Nanosecond,
		},
		{
			name:   "gemini retry delay capped",
			header: http.Header{},
			body:   `[{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"30s"}]}}]`,
			want:   maxRetryBackoff,
		},
		{
			name:   "invalid body",
			header: http.Header{},
			body:   `not json`,
			want:   0,
		},
		{
			name:   "empty body",
			header: http.Header{},
			body:   ``,
			want:   0,
		},
		{
			name:   "header takes priority over body",
			header: http.Header{"Retry-After": []string{"1"}},
			body:   `[{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"10s"}]}}]`,
			want:   1 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRetryDelay(tt.header, []byte(tt.body))
			if got != tt.want {
				t.Errorf("parseRetryDelay() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSanitizeMessagesForZAI_MergesConsecutiveUserMessages(t *testing.T) {
	gw := makeTestGateway(t)
	gw.providers["zai"] = &Provider{
		Name:      "zai",
		URL:       "https://api.z.ai/v1/chat/completions",
		APIKey:    "test-key",
		AuthStyle: "bearer",
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "You are helpful."},
			map[string]interface{}{"role": "user", "content": "First message"},
			map[string]interface{}{"role": "user", "content": "Second message"},
			map[string]interface{}{"role": "assistant", "content": "Response"},
		},
	}

	gw.sanitizeMessagesForZAI(payload)

	msgs, ok := payload["messages"].([]interface{})
	if !ok {
		t.Fatal("messages is not array")
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (system + merged_user + assistant), got %d", len(msgs))
	}

	merged, ok := msgs[1].(map[string]interface{})
	if !ok {
		t.Fatal("msg[1] is not map")
	}
	if merged["role"] != "user" {
		t.Errorf("expected user role, got %q", merged["role"])
	}
	content, _ := merged["content"].(string)
	if content != "First message\n\nSecond message" {
		t.Errorf("expected merged content, got %q", content)
	}
}

func TestSanitizeMessagesForZAI_PrependsBridgeForLeadingUser(t *testing.T) {
	gw := makeTestGateway(t)
	gw.providers["zai"] = &Provider{
		Name:      "zai",
		URL:       "https://api.z.ai/v1/chat/completions",
		APIKey:    "test-key",
		AuthStyle: "bearer",
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "Hello"},
			map[string]interface{}{"role": "assistant", "content": "Hi there"},
		},
	}

	gw.sanitizeMessagesForZAI(payload)

	msgs, ok := payload["messages"].([]interface{})
	if !ok {
		t.Fatal("messages is not array")
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}

	first, ok := msgs[0].(map[string]interface{})
	if !ok {
		t.Fatal("msg[0] is not map")
	}
	if first["role"] != "system" {
		t.Errorf("expected system role, got %q", first["role"])
	}
}

func TestSanitizeMessagesForZAI_RemovesEmptyContentMessages(t *testing.T) {
	gw := makeTestGateway(t)
	gw.providers["zai"] = &Provider{
		Name:      "zai",
		URL:       "https://api.z.ai/v1/chat/completions",
		APIKey:    "test-key",
		AuthStyle: "bearer",
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": ""},
			map[string]interface{}{"role": "assistant", "content": "Response"},
		},
	}

	gw.sanitizeMessagesForZAI(payload)

	msgs, ok := payload["messages"].([]interface{})
	if !ok {
		t.Fatal("messages is not array")
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message (empty user removed, assistant kept), got %d", len(msgs))
	}
	remaining := msgs[0].(map[string]interface{})
	if remaining["role"] != "assistant" {
		t.Errorf("expected assistant role, got %q", remaining["role"])
	}
}

func TestSanitizeToolMessagesForGemini_InjectsCachedThoughtSignature(t *testing.T) {
	gw := makeTestGateway(t)
	gw.thoughtSigCache.Store("tc-99", map[string]interface{}{
		"google": map[string]string{"thought_signature": "cached_sig"},
	})

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":       "tc-99",
						"type":     "function",
						"function": map[string]interface{}{"name": "read"},
					},
				},
			},
			map[string]interface{}{
				"role":         "tool",
				"tool_call_id": "tc-99",
				"content":      "file contents",
			},
		},
	}

	gw.sanitizeToolMessagesForGemini(payload)

	msgs := payload["messages"].([]interface{})
	assistantMsg := msgs[0].(map[string]interface{})
	toolCalls := assistantMsg["tool_calls"].([]interface{})
	tc := toolCalls[0].(map[string]interface{})

	extra, ok := tc["extra_content"]
	if !ok {
		t.Fatal("expected extra_content to be injected on tool_call")
	}
	extraMap := extra.(map[string]interface{})
	google := extraMap["google"].(map[string]string)
	if google["thought_signature"] != "cached_sig" {
		t.Errorf("expected cached_sig, got %v", google["thought_signature"])
	}
}

func TestSanitizeToolMessagesForGemini_SkipsWhenExtraContentExists(t *testing.T) {
	gw := makeTestGateway(t)
	gw.thoughtSigCache.Store("tc-99", map[string]interface{}{
		"google": map[string]string{"thought_signature": "wrong"},
	})

	existingExtra := map[string]interface{}{
		"google": map[string]string{"thought_signature": "original"},
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":            "tc-99",
						"type":          "function",
						"function":      map[string]interface{}{"name": "read"},
						"extra_content": existingExtra,
					},
				},
			},
		},
	}

	gw.sanitizeToolMessagesForGemini(payload)

	msgs := payload["messages"].([]interface{})
	assistantMsg := msgs[0].(map[string]interface{})
	toolCalls := assistantMsg["tool_calls"].([]interface{})
	tc := toolCalls[0].(map[string]interface{})

	extra := tc["extra_content"].(map[string]interface{})
	google := extra["google"].(map[string]string)
	if google["thought_signature"] != "original" {
		t.Errorf("expected original to be preserved, got %v", google["thought_signature"])
	}
}

func makeTestGateway(t *testing.T) *NenyaGateway {
	t.Helper()
	cfg := Config{
		Server:     ServerConfig{TokenRatio: 4.0},
		Governance: GovernanceConfig{ContextSoftLimit: 4000, ContextHardLimit: 24000},
		SecurityFilter: SecurityFilterConfig{
			Enabled:  true,
			Patterns: []string{`(?i)AKIA[0-9A-Z]{16}`},
		},
	}

	secrets := &SecretsConfig{
		ClientToken:  "test-token",
		ProviderKeys: map[string]string{},
	}

	logger := slog.Default()
	gw := NewNenyaGateway(cfg, secrets, logger)
	gw.providers = resolveProviders(&cfg, secrets)
	return gw
}

func TestGPT4oMaxContext(t *testing.T) {
	entry, ok := ModelRegistry["gpt-4o"]
	if !ok {
		t.Fatal("gpt-4o not found in ModelRegistry")
	}
	if entry.MaxContext != 8000 {
		t.Errorf("gpt-4o MaxContext = %d, want 8000", entry.MaxContext)
	}
	if entry.MaxOutput != 4096 {
		t.Errorf("gpt-4o MaxOutput = %d, want 4096", entry.MaxOutput)
	}
}

func TestZAIProviderDetection(t *testing.T) {
	gw := makeTestGateway(t)
	gw.providers["zai"] = &Provider{
		Name:      "zai",
		URL:       "https://api.z.ai/v1/chat/completions",
		AuthStyle: "bearer",
	}

	if !gw.isZAIProvider("zai") {
		t.Error("expected zai to be detected as z.ai provider")
	}

	gw.providers["gemini"] = &Provider{
		Name:      "gemini",
		URL:       "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
		AuthStyle: "bearer+x-goog",
	}

	if gw.isZAIProvider("gemini") {
		t.Error("expected gemini to NOT be detected as z.ai provider")
	}
}

func TestSanitizeMessagesForZAI_PreservesToolMessages(t *testing.T) {
	gw := makeTestGateway(t)
	gw.providers["zai"] = &Provider{
		Name:      "zai",
		URL:       "https://api.z.ai/v1/chat/completions",
		APIKey:    "test-key",
		AuthStyle: "bearer",
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "Read file"},
			map[string]interface{}{
				"role":       "assistant",
				"tool_calls": []interface{}{},
				"content":    "",
			},
			map[string]interface{}{
				"role":         "tool",
				"tool_call_id": "tc-1",
				"content":      "file data",
			},
			map[string]interface{}{"role": "assistant", "content": "Here's the file"},
		},
	}

	gw.sanitizeMessagesForZAI(payload)

	msgs, ok := payload["messages"].([]interface{})
	if !ok {
		t.Fatal("messages is not array")
	}

	toolMsg := msgs[3].(map[string]interface{})
	if toolMsg["role"] != "tool" {
		t.Errorf("expected tool message to be preserved, got %q", toolMsg["role"])
	}

	assistantWithCalls := msgs[2].(map[string]interface{})
	if assistantWithCalls["role"] != "assistant" {
		t.Errorf("expected assistant with tool_calls to be preserved")
	}
}

func TestParseRetryDelay_InvalidRetryAfter(t *testing.T) {
	header := http.Header{"Retry-After": []string{"not-a-number"}}
	got := parseRetryDelay(header, nil)
	if got != 0 {
		t.Errorf("expected 0 for invalid Retry-After, got %v", got)
	}
}

func TestThoughtSignatureCache_ZeroDefaults(t *testing.T) {
	cache := NewThoughtSignatureCache(0, 0)

	if cache.maxSize != 1000 {
		t.Errorf("expected default maxSize 1000, got %d", cache.maxSize)
	}
	if cache.defaultTTL != 10*time.Minute {
		t.Errorf("expected default TTL 10m, got %v", cache.defaultTTL)
	}
}

func TestGeminiTransformer_ExtractsExtraContentMultipleToolCalls(t *testing.T) {
	var captured []struct {
		id    string
		extra interface{}
	}

	transformer := &GeminiTransformer{
		onExtraContent: func(toolCallID string, extraContent interface{}) {
			captured = append(captured, struct {
				id    string
				extra interface{}
			}{toolCallID, extraContent})
		},
	}

	input := `{"choices":[{"delta":{"tool_calls":[{"id":"tc-a","type":"function","function":{"name":"read"},"extra_content":{"google":{"thought_signature":"sigA"}}},{"id":"tc-b","type":"function","function":{"name":"write"},"extra_content":{"google":{"thought_signature":"sigB"}}}]}}]}`

	_, err := transformer.TransformSSEChunk([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(captured) != 2 {
		t.Fatalf("expected 2 captures, got %d", len(captured))
	}
	if captured[0].id != "tc-a" {
		t.Errorf("expected tc-a, got %q", captured[0].id)
	}
	if captured[1].id != "tc-b" {
		t.Errorf("expected tc-b, got %q", captured[1].id)
	}
}

func TestThoughtSignatureCache_OverwriteExisting(t *testing.T) {
	cache := NewThoughtSignatureCache(100, 1*time.Minute)

	cache.Store("key", "value1")
	cache.Store("key", "value2")

	val, ok := cache.Load("key")
	if !ok {
		t.Fatal("expected key to exist")
	}
	if val != "value2" {
		t.Errorf("expected value2, got %v", val)
	}
}

func TestParseRetryDelay_BodyArrayWithNoDetails(t *testing.T) {
	body := `[{"error":{"code":429,"message":"rate limited"}}]`
	got := parseRetryDelay(http.Header{}, []byte(body))
	if got != 0 {
		t.Errorf("expected 0 when no retry details, got %v", got)
	}
}

func TestCallEngine_SelectsOllamaClient(t *testing.T) {
	gw := makeTestGateway(t)
	gw.providers["ollama"] = &Provider{
		Name:      "ollama",
		URL:       "http://127.0.0.1:11434/v1/chat/completions",
		AuthStyle: "none",
		ApiFormat: "ollama",
	}

	engine := EngineConfig{
		Provider: "ollama",
		Model:    "qwen2.5-coder:7b",
	}

	p, ok := gw.providers[engine.Provider]
	if !ok {
		t.Fatal("ollama provider not found")
	}

	apiFormat := p.ApiFormat
	if apiFormat == "" {
		apiFormat = "openai"
	}

	expectedClient := gw.client
	if apiFormat == "ollama" {
		expectedClient = gw.ollamaClient
	}

	if expectedClient == nil {
		t.Fatal("expected non-nil client")
	}

	if apiFormat == "ollama" && expectedClient != gw.ollamaClient {
		t.Error("expected ollamaClient for ollama format")
	}
	if apiFormat != "ollama" && expectedClient != gw.client {
		t.Error("expected client for non-ollama format")
	}
}

func TestCallEngine_NonOllamaUsesDefaultClient(t *testing.T) {
	gw := makeTestGateway(t)
	gw.providers["ollama"] = &Provider{
		Name:      "ollama",
		URL:       "http://127.0.0.1:11434/v1/chat/completions",
		AuthStyle: "none",
		ApiFormat: "openai",
	}

	engine := EngineConfig{
		Provider: "ollama",
		Model:    "qwen2.5-coder:7b",
	}

	p, ok := gw.providers[engine.Provider]
	if !ok {
		t.Fatal("provider not found")
	}

	apiFormat := p.ApiFormat
	if apiFormat == "" {
		apiFormat = "openai"
	}

	if apiFormat != "ollama" {
		if gw.client == nil {
			t.Error("expected default client for non-ollama format")
		}
	}
}
