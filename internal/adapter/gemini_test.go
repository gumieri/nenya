package adapter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type mockThoughtSigCache struct {
	mu     sync.Mutex
	data   map[string]interface{}
	loaded map[string]bool
}

func newMockThoughtSigCache() *mockThoughtSigCache {
	return &mockThoughtSigCache{
		data:   make(map[string]interface{}),
		loaded: make(map[string]bool),
	}
}

func (m *mockThoughtSigCache) Load(key string) (interface{}, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loaded[key] = true
	v, ok := m.data[key]
	return v, ok
}

func (m *mockThoughtSigCache) Store(key string, value interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
}

type mockGeminiLogger struct {
	debugs []string
	warns  []string
}

func (m *mockGeminiLogger) Debug(msg string, args ...any) {
	m.debugs = append(m.debugs, msg)
}

func (m *mockGeminiLogger) Warn(msg string, args ...any) {
	m.warns = append(m.warns, msg)
}

func extractContentStr(msg map[string]interface{}) string {
	if c, ok := msg["content"].(string); ok {
		return c
	}
	return ""
}

func TestGeminiAdapter_MutateRequest_EmptyBody(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{})
	out, err := a.MutateRequest(nil, "gemini-pro", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil output for nil input")
	}
}

func TestGeminiAdapter_MutateRequest_NoModelMapChange(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{})
	body := []byte(`{"model":"unknown-model-xyz","messages":[{"role":"user","content":"hi"}]}`)
	out, err := a.MutateRequest(body, "unknown-model-xyz", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected identity transform for unmapped model, got: %s", string(out))
	}
}

func TestGeminiAdapter_MutateRequest_ModelMapCaseInsensitive(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{
		ModelMap: GeminiModelMap,
	})
	body := []byte(`{"model":"GEMINI-FLASH","messages":[{"role":"user","content":"hi"}]}`)
	out, err := a.MutateRequest(body, "GEMINI-FLASH", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if m["model"] != "gemini-2.5-flash" {
		t.Errorf("expected model=gemini-2.5-flash, got %v", m["model"])
	}
}

func TestGeminiAdapter_MutateRequest_StripStreamOptions(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{})
	body := []byte(`{"model":"gemini-pro","stream_options":{"include_usage":true}}`)
	out, err := a.MutateRequest(body, "gemini-pro", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if _, ok := m["stream_options"]; ok {
		t.Error("stream_options should have been stripped")
	}
}

func TestGeminiAdapter_MutateRequest_UnchangedBody(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{})
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out, err := a.MutateRequest(body, "", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected identity transform, got: %s", string(out))
	}
}

func TestGeminiAdapter_InjectAuth(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	err := a.InjectAuth(req, "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("expected 'Bearer test-key', got %q", got)
	}
	if got := req.Header.Get("x-goog-api-key"); got != "test-key" {
		t.Errorf("expected 'test-key', got %q", got)
	}
}

func TestGeminiAdapter_InjectAuth_EmptyKey(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	err := a.InjectAuth(req, "")
	if err == nil {
		t.Error("expected error for empty API key")
	}
}

func TestGeminiAdapter_MutateResponse_EmptyBody(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{})
	out, err := a.MutateResponse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil output for nil input")
	}
}

func TestGeminiAdapter_MutateResponse_NonJSON(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{})
	body := []byte(`[{"key":"value"}]`)
	out, err := a.MutateResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected identity transform for JSON array, got: %s", string(out))
	}
}

func TestGeminiAdapter_MutateResponse_NoChoices(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{})
	body := []byte(`{"key":"value"}`)
	out, err := a.MutateResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected identity transform when no choices, got: %s", string(out))
	}
}

func TestGeminiAdapter_MutateResponse_NoToolCalls(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{})
	body := []byte(`{"choices":[{"delta":{"content":"hello"}}]}`)
	out, err := a.MutateResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected identity transform when no tool_calls, got: %s", string(out))
	}
}

func TestGeminiAdapter_MutateResponse_EnrichToolCalls(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{})
	body := []byte(`{"choices":[{"delta":{"tool_calls":[{"id":"tc1","function":{"name":"fn1","arguments":"{}"}},{"id":"tc2","function":{"name":"fn2","arguments":"{}"}}]}}]}`)
	out, err := a.MutateResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	choices := m["choices"].([]interface{})
	delta := choices[0].(map[string]interface{})["delta"].(map[string]interface{})
	tcs := delta["tool_calls"].([]interface{})
	for i, tc := range tcs {
		tcMap := tc.(map[string]interface{})
		if idx, ok := tcMap["index"]; !ok || idx.(float64) != float64(i) {
			t.Errorf("tool_call[%d]: expected index=%d, got %v", i, i, idx)
		}
	}
}

func TestGeminiAdapter_NormalizeError(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{})
	tests := []struct {
		name string
		code int
		body string
		want ErrorClass
	}{
		{"rate_limited", 429, "", ErrorRateLimited},
		{"server_error_500", 500, "", ErrorRetryable},
		{"server_error_502", 502, "", ErrorRetryable},
		{"server_error_503", 503, "", ErrorRetryable},
		{"server_error_504", 504, "", ErrorRetryable},
		{"400_resource_exhausted", 400, `resource_exhausted`, ErrorRetryable},
		{"400_response_blocked", 400, `the response was blocked`, ErrorRetryable},
		{"400_content_no_parts", 400, `content has no parts`, ErrorRetryable},
		{"400_quota_exceeded", 400, `quota exceeded`, ErrorRetryable},
		{"400_overloaded", 400, `overloaded`, ErrorRetryable},
		{"400_context_length", 400, `context_length_exceeded`, ErrorRetryable},
		{"400_permanent", 400, `invalid request`, ErrorPermanent},
		{"400_empty_body", 400, ``, ErrorPermanent},
		{"413_overloaded", 413, `overloaded`, ErrorRetryable},
		{"413_unknown", 413, `unknown error`, ErrorPermanent},
		{"422_overloaded", 422, `overloaded`, ErrorRetryable},
		{"422_unknown", 422, `unknown error`, ErrorPermanent},
		{"403_forbidden", 403, ``, ErrorPermanent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := a.NormalizeError(tt.code, []byte(tt.body))
			if got != tt.want {
				t.Errorf("NormalizeError(%d, %q) = %v, want %v", tt.code, tt.body, got, tt.want)
			}
		})
	}
}

func TestGeminiAdapter_Sanitize_OrphanedToolCalls(t *testing.T) {
	cache := newMockThoughtSigCache()
	logger := &mockGeminiLogger{}
	a := NewGeminiAdapter(GeminiAdapterDeps{
		ThoughtSigCache: cache,
		ExtractContent:  extractContentStr,
		Logger:          logger,
	})
	body := []byte(`{
		"model": "gemini-pro",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "", "tool_calls": [
				{"id": "tc1", "type": "function", "function": {"name": "get_weather", "arguments": "{}"}},
				{"id": "tc2", "type": "function", "function": {"name": "get_time", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "tc1", "content": "sunny"},
			{"role": "tool", "tool_call_id": "tc2", "content": "12:00"}
		]
	}`)
	out, err := a.MutateRequest(body, "gemini-pro", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	msgs := m["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Errorf("expected 1 message (all orphaned, empty assistant dropped), got %d", len(msgs))
	}
	if len(logger.warns) != 2 {
		t.Errorf("expected 2 warns, got %d: %v", len(logger.warns), logger.warns)
	}
}

func TestGeminiAdapter_Sanitize_WithOrphanedToolCalls(t *testing.T) {
	cache := newMockThoughtSigCache()
	cache.data["tc1"] = "thought1"
	logger := &mockGeminiLogger{}
	a := NewGeminiAdapter(GeminiAdapterDeps{
		ThoughtSigCache: cache,
		ExtractContent:  extractContentStr,
		Logger:          logger,
	})
	body := []byte(`{
		"model": "gemini-pro",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "", "tool_calls": [
				{"id": "tc1", "type": "function", "function": {"name": "get_weather", "arguments": "{}"}, "extra_content": "thought1"},
				{"id": "tc2", "type": "function", "function": {"name": "get_time", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "tc1", "content": "sunny"},
			{"role": "tool", "tool_call_id": "tc2", "content": "12:00"}
		]
	}`)
	out, err := a.MutateRequest(body, "gemini-pro", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	msgs := m["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Errorf("expected 3 messages (orphaned tc2 pair stripped), got %d", len(msgs))
	}
	if len(logger.warns) != 1 {
		t.Errorf("expected 1 warn, got %d: %v", len(logger.warns), logger.warns)
	}
}

func TestGeminiAdapter_Sanitize_InjectToolNames(t *testing.T) {
	cache := newMockThoughtSigCache()
	a := NewGeminiAdapter(GeminiAdapterDeps{
		ThoughtSigCache: cache,
		ExtractContent:  extractContentStr,
	})
	body := []byte(`{
		"model": "gemini-pro",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "", "tool_calls": [
				{"id": "tc1", "type": "function", "function": {"name": "get_weather", "arguments": "{}"}, "extra_content": "thought1"}
			]},
			{"role": "tool", "tool_call_id": "tc1", "content": "sunny"}
		]
	}`)
	out, err := a.MutateRequest(body, "gemini-pro", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	msgs := m["messages"].([]interface{})
	toolMsg := msgs[2].(map[string]interface{})
	if name, ok := toolMsg["name"]; !ok || name != "get_weather" {
		t.Errorf("expected tool message name='get_weather', got %v", name)
	}
}

func TestGeminiAdapter_Sanitize_NoExtraContentLoadsFromCache(t *testing.T) {
	cache := newMockThoughtSigCache()
	cache.data["tc1"] = "injected-thought"
	logger := &mockGeminiLogger{}
	a := NewGeminiAdapter(GeminiAdapterDeps{
		ThoughtSigCache: cache,
		ExtractContent:  extractContentStr,
		Logger:          logger,
	})
	body := []byte(`{
		"model": "gemini-pro",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "", "tool_calls": [
				{"id": "tc1", "type": "function", "function": {"name": "get_weather", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "tc1", "content": "sunny"}
		]
	}`)
	out, err := a.MutateRequest(body, "gemini-pro", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	msgs := m["messages"].([]interface{})
	assistantMsg := msgs[1].(map[string]interface{})
	tcs := assistantMsg["tool_calls"].([]interface{})
	tc := tcs[0].(map[string]interface{})
	if extra, ok := tc["extra_content"]; !ok || extra != "injected-thought" {
		t.Errorf("expected extra_content='injected-thought', got %v", extra)
	}
}

func TestGeminiAdapter_Sanitize_EmptyAssistantDropped(t *testing.T) {
	cache := newMockThoughtSigCache()
	a := NewGeminiAdapter(GeminiAdapterDeps{
		ThoughtSigCache: cache,
		ExtractContent:  extractContentStr,
	})
	body := []byte(`{
		"model": "gemini-pro",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "", "tool_calls": [
				{"id": "tc1", "type": "function", "function": {"name": "get_weather", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "tc1", "content": "sunny"}
		]
	}`)
	out, err := a.MutateRequest(body, "gemini-pro", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	msgs := m["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Errorf("expected 1 message (all stripped), got %d", len(msgs))
	}
}

func TestGeminiAdapter_Sanitize_AssistantWithContentKept(t *testing.T) {
	cache := newMockThoughtSigCache()
	a := NewGeminiAdapter(GeminiAdapterDeps{
		ThoughtSigCache: cache,
		ExtractContent:  extractContentStr,
	})
	body := []byte(`{
		"model": "gemini-pro",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "I will help", "tool_calls": [
				{"id": "tc1", "type": "function", "function": {"name": "get_weather", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "tc1", "content": "sunny"}
		]
	}`)
	out, err := a.MutateRequest(body, "gemini-pro", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	msgs := m["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Errorf("expected 2 messages (assistant kept with content), got %d", len(msgs))
	}
	assistantMsg := msgs[1].(map[string]interface{})
	if _, ok := assistantMsg["tool_calls"]; ok {
		t.Error("expected tool_calls to be removed from assistant with content")
	}
}

func TestGeminiAdapter_Sanitize_ToolMessageSyntheticName(t *testing.T) {
	cache := newMockThoughtSigCache()
	logger := &mockGeminiLogger{}
	a := NewGeminiAdapter(GeminiAdapterDeps{
		ThoughtSigCache: cache,
		ExtractContent:  extractContentStr,
		Logger:          logger,
	})
	body := []byte(`{
		"model": "gemini-pro",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "", "tool_calls": [
				{"id": "unknown_id", "type": "function", "function": {"name": "get_weather", "arguments": "{}"}, "extra_content": "thought"}
			]},
			{"role": "tool", "tool_call_id": "unknown_id", "content": "sunny"}
		]
	}`)
	out, err := a.MutateRequest(body, "gemini-pro", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	msgs := m["messages"].([]interface{})
	toolMsg := msgs[2].(map[string]interface{})
	if name, ok := toolMsg["name"]; !ok || name != "get_weather" {
		t.Errorf("expected tool message name='get_weather', got %v", name)
	}
}

func TestGeminiAdapter_Sanitize_ToolMessageKnownIDButNoName(t *testing.T) {
	cache := newMockThoughtSigCache()
	logger := &mockGeminiLogger{}
	a := NewGeminiAdapter(GeminiAdapterDeps{
		ThoughtSigCache: cache,
		ExtractContent:  extractContentStr,
		Logger:          logger,
	})
	body := []byte(`{
		"model": "gemini-pro",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "", "tool_calls": [
				{"id": "tc_no_name", "type": "function", "function": {"name": "", "arguments": "{}"}, "extra_content": "thought"}
			]},
			{"role": "tool", "tool_call_id": "tc_no_name", "content": "result"}
		]
	}`)
	out, err := a.MutateRequest(body, "gemini-pro", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	msgs := m["messages"].([]interface{})
	toolMsg := msgs[2].(map[string]interface{})
	name, hasName := toolMsg["name"]
	if hasName {
		t.Errorf("expected no name set when function name empty, got %v", name)
	}
}

func TestGeminiAdapter_Sanitize_ToolMessageAlreadyHasName(t *testing.T) {
	cache := newMockThoughtSigCache()
	a := NewGeminiAdapter(GeminiAdapterDeps{
		ThoughtSigCache: cache,
		ExtractContent:  extractContentStr,
	})
	body := []byte(`{
		"model": "gemini-pro",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "", "tool_calls": [
				{"id": "tc1", "type": "function", "function": {"name": "get_weather", "arguments": "{}"}, "extra_content": "thought"}
			]},
			{"role": "tool", "tool_call_id": "tc1", "name": "existing_name", "content": "sunny"}
		]
	}`)
	out, err := a.MutateRequest(body, "gemini-pro", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	msgs := m["messages"].([]interface{})
	toolMsg := msgs[2].(map[string]interface{})
	if name, ok := toolMsg["name"]; !ok || name != "existing_name" {
		t.Errorf("expected name='existing_name', got %v", name)
	}
}

func TestGeminiAdapter_Sanitize_NoMessages(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{
		ModelMap: map[string]string{},
	})
	body := []byte(`{"model":"gemini-pro"}`)
	out, err := a.MutateRequest(body, "gemini-pro", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected identity transform, got: %s", string(out))
	}
}

func TestGeminiAdapter_Sanitize_NonAssistantToolCalls(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{
		ExtractContent: extractContentStr,
		ModelMap:       map[string]string{},
	})
	body := []byte(`{
		"model": "gemini-pro",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "user", "content": "world"}
		]
	}`)
	out, err := a.MutateRequest(body, "gemini-pro", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected identity for non-tool messages, got: %s", string(out))
	}
}

func TestGeminiAdapter_Sanitize_NoThoughtSigCache(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{
		ExtractContent: extractContentStr,
		ModelMap:       map[string]string{},
	})
	body := []byte(`{
		"model": "gemini-pro",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "", "tool_calls": [
				{"id": "tc1", "type": "function", "function": {"name": "get_weather", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "tc1", "content": "sunny"}
		]
	}`)
	out, err := a.MutateRequest(body, "gemini-pro", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected identity (no sanitize without cache), got: %s", string(out))
	}
}

func TestGeminiAdapter_StoreExtraContent_NilCache(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{})
	tcMap := map[string]interface{}{"id": "tc1", "extra_content": "data"}
	changed := a.storeExtraContent(tcMap)
	if changed {
		t.Error("expected changed=false when cache is nil (even with extra_content)")
	}
}

func TestGeminiAdapter_StoreExtraContent_NoID(t *testing.T) {
	cache := newMockThoughtSigCache()
	a := NewGeminiAdapter(GeminiAdapterDeps{ThoughtSigCache: cache})
	tcMap := map[string]interface{}{"extra_content": "data"}
	changed := a.storeExtraContent(tcMap)
	if changed {
		t.Error("expected changed=false when no ID")
	}
}

func TestGeminiAdapter_StoreExtraContent_NoExtra(t *testing.T) {
	cache := newMockThoughtSigCache()
	a := NewGeminiAdapter(GeminiAdapterDeps{ThoughtSigCache: cache})
	tcMap := map[string]interface{}{"id": "tc1"}
	changed := a.storeExtraContent(tcMap)
	if changed {
		t.Error("expected changed=false when no extra_content")
	}
}

func TestGeminiAdapter_StoreExtraContent_WithCache(t *testing.T) {
	cache := newMockThoughtSigCache()
	a := NewGeminiAdapter(GeminiAdapterDeps{ThoughtSigCache: cache})
	tcMap := map[string]interface{}{"id": "tc1", "extra_content": "thought-data"}
	changed := a.storeExtraContent(tcMap)
	if !changed {
		t.Error("expected changed=true")
	}
	if cached, ok := cache.data["tc1"]; !ok || cached != "thought-data" {
		t.Errorf("expected cached 'thought-data', got %v", cached)
	}
}
