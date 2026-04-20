package adapter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIAdapter_MutateRequest_Identity(t *testing.T) {
	a := NewOpenAIAdapter(Capabilities{
		StreamOptions:  true,
		AutoToolChoice: true,
		ContentArrays:  true,
	})
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":true},"tool_choice":"auto"}`)
	out, err := a.MutateRequest(body, "gpt-4", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected identity transform, got: %s", string(out))
	}
}

func TestOpenAIAdapter_MutateRequest_StripStreamOptions(t *testing.T) {
	a := NewOpenAIAdapter(Capabilities{StreamOptions: false})
	body := []byte(`{"model":"gpt-4","stream_options":{"include_usage":true}}`)
	out, err := a.MutateRequest(body, "gpt-4", true)
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

func TestOpenAIAdapter_MutateRequest_StripAutoToolChoice(t *testing.T) {
	a := NewOpenAIAdapter(Capabilities{AutoToolChoice: false})
	body := []byte(`{"model":"gpt-4","tool_choice":"auto"}`)
	out, err := a.MutateRequest(body, "gpt-4", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if _, ok := m["tool_choice"]; ok {
		t.Error("tool_choice should have been stripped")
	}
}

func TestOpenAIAdapter_MutateRequest_StripAutoToolChoice_NonAuto(t *testing.T) {
	a := NewOpenAIAdapter(Capabilities{AutoToolChoice: false})
	body := []byte(`{"model":"gpt-4","tool_choice":"required"}`)
	out, err := a.MutateRequest(body, "gpt-4", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if _, ok := m["tool_choice"]; !ok {
		t.Error("tool_choice 'required' should NOT have been stripped")
	}
}

func TestOpenAIAdapter_MutateRequest_FlattenContentArrays(t *testing.T) {
	a := NewOpenAIAdapter(Capabilities{ContentArrays: false})
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"text","text":"world"}]}]}`)
	out, err := a.MutateRequest(body, "gpt-4", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	msgs := m["messages"].([]interface{})
	firstMsg := msgs[0].(map[string]interface{})
	content, ok := firstMsg["content"].(string)
	if !ok {
		t.Fatalf("expected content to be string, got %T", firstMsg["content"])
	}
	if content != "hello\nworld" {
		t.Errorf("expected 'hello\\nworld', got %q", content)
	}
}

func TestOpenAIAdapter_MutateRequest_EmptyBody(t *testing.T) {
	a := NewOpenAIAdapter(Capabilities{})
	out, err := a.MutateRequest(nil, "gpt-4", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil output for nil input")
	}
}

func TestOpenAIAdapter_InjectAuth(t *testing.T) {
	a := NewOpenAIAdapter(Capabilities{})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	err := a.InjectAuth(req, "test-key-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer test-key-123" {
		t.Errorf("expected 'Bearer test-key-123', got %q", got)
	}
}

func TestOpenAIAdapter_InjectAuth_EmptyKey(t *testing.T) {
	a := NewOpenAIAdapter(Capabilities{})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	err := a.InjectAuth(req, "")
	if err == nil {
		t.Error("expected error for empty API key")
	}
}

func TestBearerPlusGoogAuth(t *testing.T) {
	a := &BearerPlusGoogAuth{}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	err := a.InjectAuth(req, "key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer key" {
		t.Errorf("expected 'Bearer key', got %q", got)
	}
	if got := req.Header.Get("x-goog-api-key"); got != "key" {
		t.Errorf("expected 'key', got %q", got)
	}
}

func TestNoAuthAdapter_InjectAuth(t *testing.T) {
	a := NewNoAuthAdapter(Capabilities{})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	err := a.InjectAuth(req, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("expected no Authorization header, got %q", got)
	}
}

func TestOpenAIAdapter_NormalizeError(t *testing.T) {
	a := NewOpenAIAdapter(Capabilities{})

	tests := []struct {
		code     int
		body     string
		expected ErrorClass
	}{
		{429, "", ErrorRateLimited},
		{500, "", ErrorRetryable},
		{502, "", ErrorRetryable},
		{503, "", ErrorRetryable},
		{400, `{"error":"invalid request"}`, ErrorPermanent},
		{400, `{"error":"model_overloaded"}`, ErrorRetryable},
		{400, `{"error":"context_length_exceeded"}`, ErrorRetryable},
		{404, "", ErrorPermanent},
	}

	for _, tt := range tests {
		got := a.NormalizeError(tt.code, []byte(tt.body))
		if got != tt.expected {
			t.Errorf("NormalizeError(%d, %q) = %s, want %s", tt.code, tt.body, got, tt.expected)
		}
	}
}

func TestOllamaAdapter_InjectAuth(t *testing.T) {
	a := NewOllamaAdapter()

	t.Run("with key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		err := a.InjectAuth(req, "my-key")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer my-key" {
			t.Errorf("expected 'Bearer my-key', got %q", got)
		}
	})

	t.Run("without key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		err := a.InjectAuth(req, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := req.Header.Get("Authorization"); got != "" {
			t.Errorf("expected no header, got %q", got)
		}
	})
}

func TestGeminiAdapter_MutateResponse_AddIndex(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{})
	body := []byte(`{"choices":[{"delta":{"tool_calls":[{"id":"tc1","function":{"name":"fn1","arguments":"{}"}}]}}]}`)
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
	tc := tcs[0].(map[string]interface{})
	if idx, ok := tc["index"]; !ok || idx.(float64) != 0 {
		t.Errorf("expected index=0, got %v", idx)
	}
}

func TestGeminiAdapter_MutateResponse_Identity(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{})
	body := []byte(`{"choices":[{"delta":{"content":"hello"}}]}`)
	out, err := a.MutateResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected identity transform, got: %s", string(out))
	}
}

func TestGeminiAdapter_ModelMap(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{
		ModelMap: GeminiModelMap,
	})
	body := []byte(`{"model":"gemini-flash","messages":[{"role":"user","content":"hi"}]}`)
	out, err := a.MutateRequest(body, "gemini-flash", true)
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

func TestAdapterForAuthStyle(t *testing.T) {
	tests := []struct {
		style    string
		wantAuth string
	}{
		{"none", ""},
		{"bearer", "Bearer key"},
		{"bearer+x-goog", "Bearer key"},
		{"unknown", "Bearer key"},
	}

	for _, tt := range tests {
		a := AdapterForAuthStyle(tt.style)
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		if err := a.InjectAuth(req, "key"); err != nil {
			t.Fatalf("InjectAuth() error = %v", err)
		}
		got := req.Header.Get("Authorization")
		if got != tt.wantAuth {
			t.Errorf("AdapterForAuthStyle(%q).InjectAuth() Authorization = %q, want %q", tt.style, got, tt.wantAuth)
		}
		if tt.style == "bearer+x-goog" {
			if got := req.Header.Get("x-goog-api-key"); got != "key" {
				t.Errorf("expected x-goog-api-key for bearer+x-goog, got %q", got)
			}
		}
	}
}

func TestRegistry_ForProvider_Unknown(t *testing.T) {
	a := ForProvider("completely-unknown-provider")
	if a == nil {
		t.Error("expected default adapter, got nil")
	}
	if _, ok := a.(*OpenAIAdapter); !ok {
		t.Errorf("expected *OpenAIAdapter, got %T", a)
	}
}

func TestRegistry_ForProvider_Known(t *testing.T) {
	a := ForProvider("ollama")
	if _, ok := a.(*OllamaAdapter); !ok {
		t.Errorf("expected *OllamaAdapter, got %T", a)
	}
}

func TestErrorClass_String(t *testing.T) {
	tests := []struct {
		e    ErrorClass
		want string
	}{
		{ErrorPermanent, "permanent"},
		{ErrorRetryable, "retryable"},
		{ErrorRateLimited, "rate_limited"},
		{ErrorQuotaExhausted, "quota_exhausted"},
	}
	for _, tt := range tests {
		got := tt.e.String()
		if got != tt.want {
			t.Errorf("ErrorClass(%d).String() = %q, want %q", tt.e, got, tt.want)
		}
	}
}

func TestFlattenContentArrays_MixedTypes(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "hello"},
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "http://example.com/img.png"}},
				map[string]interface{}{"type": "text", "text": "world"},
			},
		},
	}
	changed := flattenContentArrays(msgs)
	if !changed {
		t.Error("expected changed=true")
	}
	msg := msgs[0].(map[string]interface{})
	content, ok := msg["content"].(string)
	if !ok {
		t.Fatalf("expected string content, got %T", msg["content"])
	}
	if !strings.Contains(content, "hello") || !strings.Contains(content, "world") {
		t.Errorf("expected content to contain hello and world, got %q", content)
	}
}

func TestZAIAdapter_MutateRequest_SkipsWhenToolsPresent(t *testing.T) {
	a := NewZAIAdapter(ZAIAdapterDeps{
		ExtractContent: func(msg map[string]interface{}) string {
			if c, ok := msg["content"].(string); ok {
				return c
			}
			return ""
		},
	})

	body := []byte(`{
		"model": "glm-5-turbo",
		"messages": [
			{"role": "user", "content": "first part"},
			{"role": "user", "content": "second part"},
			{"role": "assistant", "content": "response"}
		],
		"tools": [{"type": "function", "function": {"name": "read_file"}}]
	}`)
	out, err := a.MutateRequest(body, "glm-5-turbo", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected identity transform when tools present, got: %s", string(out))
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}
	msgs := m["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Errorf("expected 3 messages (untouched), got %d", len(msgs))
	}
}

func TestZAIAdapter_MutateRequest_MutatesWhenNoTools(t *testing.T) {
	a := NewZAIAdapter(ZAIAdapterDeps{
		ExtractContent: func(msg map[string]interface{}) string {
			if c, ok := msg["content"].(string); ok {
				return c
			}
			return ""
		},
	})

	body := []byte(`{
		"model": "glm-5-turbo",
		"messages": [
			{"role": "user", "content": "first part"},
			{"role": "user", "content": "second part"}
		]
	}`)
	out, err := a.MutateRequest(body, "glm-5-turbo", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) == string(body) {
		t.Error("expected body to be mutated (user messages merged, system bridge added) when no tools present")
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}
	msgs := m["messages"].([]interface{})
	if len(msgs) < 2 {
		t.Errorf("expected at least 2 messages after sanitization, got %d", len(msgs))
	}
	if msgs[0].(map[string]interface{})["role"] != "system" {
		t.Error("expected system bridge as first message")
	}
}
