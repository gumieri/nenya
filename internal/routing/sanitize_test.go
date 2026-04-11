package routing

import (
	"log/slog"
	"os"
	"testing"

	"nenya/internal/config"
)

func defaultSanitizeDeps() TransformDeps {
	return TransformDeps{
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Config: &config.Config{},
		ExtractContentText: func(msg map[string]interface{}) string {
			if c, ok := msg["content"].(string); ok {
				return c
			}
			return ""
		},
	}
}

func TestSanitizePayload_StripStreamOptions(t *testing.T) {
	providers := map[string]*config.Provider{
		"nvidia": {Name: "nvidia"},
		"gemini": {Name: "gemini"},
	}
	deps := defaultSanitizeDeps()
	deps.Providers = providers

	payload := map[string]interface{}{
		"model": "nemotron-3-super",
		"stream_options": map[string]interface{}{
			"include_usage": true,
		},
	}
	SanitizePayload(deps, payload, "nvidia")
	if _, ok := payload["stream_options"]; ok {
		t.Fatal("stream_options should be stripped for nvidia")
	}

	payload2 := map[string]interface{}{
		"model": "gemini-2.5-flash",
		"stream_options": map[string]interface{}{
			"include_usage": true,
		},
	}
	SanitizePayload(deps, payload2, "gemini")
	if _, ok := payload2["stream_options"]; ok {
		t.Fatal("stream_options should be stripped for gemini")
	}
}

func TestSanitizePayload_KeepStreamOptions(t *testing.T) {
	providers := map[string]*config.Provider{
		"deepseek": {Name: "deepseek"},
	}
	deps := defaultSanitizeDeps()
	deps.Providers = providers

	payload := map[string]interface{}{
		"model": "deepseek-reasoner",
		"stream_options": map[string]interface{}{
			"include_usage": true,
		},
	}
	SanitizePayload(deps, payload, "deepseek")
	if _, ok := payload["stream_options"]; !ok {
		t.Fatal("stream_options should be kept for deepseek")
	}
}

func TestSanitizePayload_StripAutoToolChoice(t *testing.T) {
	providers := map[string]*config.Provider{
		"nvidia": {Name: "nvidia"},
	}
	deps := defaultSanitizeDeps()
	deps.Providers = providers

	payload := map[string]interface{}{
		"model":       "nemotron-3-super",
		"tool_choice": "auto",
	}
	SanitizePayload(deps, payload, "nvidia")
	if _, ok := payload["tool_choice"]; ok {
		t.Fatal("tool_choice \"auto\" should be stripped for nvidia")
	}
}

func TestSanitizePayload_KeepAutoToolChoice(t *testing.T) {
	providers := map[string]*config.Provider{
		"deepseek": {Name: "deepseek"},
	}
	deps := defaultSanitizeDeps()
	deps.Providers = providers

	payload := map[string]interface{}{
		"model":       "deepseek-reasoner",
		"tool_choice": "auto",
	}
	SanitizePayload(deps, payload, "deepseek")
	if v, ok := payload["tool_choice"]; !ok || v != "auto" {
		t.Fatal("tool_choice \"auto\" should be kept for deepseek")
	}
}

func TestSanitizePayload_StripNonStringToolChoice(t *testing.T) {
	providers := map[string]*config.Provider{
		"nvidia": {Name: "nvidia"},
	}
	deps := defaultSanitizeDeps()
	deps.Providers = providers

	payload := map[string]interface{}{
		"model":       "nemotron-3-super",
		"tool_choice": map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "foo"}},
	}
	SanitizePayload(deps, payload, "nvidia")
	if _, ok := payload["tool_choice"]; !ok {
		t.Fatal("non-string tool_choice (object) should be kept")
	}
}

func TestSanitizePayload_FlattenContentArrays(t *testing.T) {
	providers := map[string]*config.Provider{
		"nvidia": {Name: "nvidia"},
	}
	deps := defaultSanitizeDeps()
	deps.Providers = providers

	payload := map[string]interface{}{
		"model": "nemotron-3-super",
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "hello"},
					map[string]interface{}{"type": "text", "text": "world"},
				},
			},
		},
	}
	SanitizePayload(deps, payload, "nvidia")
	msgs := payload["messages"].([]interface{})
	content := msgs[0].(map[string]interface{})["content"].(string)
	if content != "hello\nworld" {
		t.Fatalf("expected flattened content, got %q", content)
	}
}

func TestSanitizePayload_KeepContentArrays(t *testing.T) {
	providers := map[string]*config.Provider{
		"deepseek": {Name: "deepseek"},
	}
	deps := defaultSanitizeDeps()
	deps.Providers = providers

	contentArr := []interface{}{
		map[string]interface{}{"type": "text", "text": "hello"},
	}
	payload := map[string]interface{}{
		"model":    "deepseek-reasoner",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": contentArr}},
	}
	SanitizePayload(deps, payload, "deepseek")
	msgs := payload["messages"].([]interface{})
	arr, ok := msgs[0].(map[string]interface{})["content"].([]interface{})
	if !ok || len(arr) != 1 {
		t.Fatal("content array should be kept for deepseek")
	}
}

func TestSanitizePayload_NoMessages(t *testing.T) {
	providers := map[string]*config.Provider{
		"nvidia": {Name: "nvidia"},
	}
	deps := defaultSanitizeDeps()
	deps.Providers = providers

	payload := map[string]interface{}{
		"model": "nemotron-3-super",
	}
	SanitizePayload(deps, payload, "nvidia")
}

func TestFlattenContentArray(t *testing.T) {
	tests := []struct {
		name  string
		input []interface{}
		want  string
	}{
		{"text blocks", []interface{}{
			map[string]interface{}{"type": "text", "text": "hello"},
			map[string]interface{}{"type": "text", "text": "world"},
		}, "hello\nworld"},
		{"single text", []interface{}{
			map[string]interface{}{"type": "text", "text": "only"},
		}, "only"},
		{"mixed with image", []interface{}{
			map[string]interface{}{"type": "text", "text": "desc"},
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "http://example.com/img.png"}},
		}, "desc"},
		{"empty", []interface{}{}, ""},
		{"non-map entries", []interface{}{"not-a-map"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := flattenContentArray(tt.input)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitizePayload_ToolCallsPassthrough(t *testing.T) {
	providers := map[string]*config.Provider{
		"nvidia": {Name: "nvidia"},
	}
	deps := defaultSanitizeDeps()
	deps.Providers = providers

	toolCalls := []interface{}{
		map[string]interface{}{
			"id":   "call_123",
			"type": "function",
			"function": map[string]interface{}{
				"name":      "read_file",
				"arguments": "{\"path\":\"/tmp/test.go\"}",
			},
		},
	}

	payload := map[string]interface{}{
		"model": "nemotron-3-super",
		"messages": []interface{}{
			map[string]interface{}{
				"role":       "assistant",
				"content":    "",
				"tool_calls": toolCalls,
			},
			map[string]interface{}{
				"role":         "tool",
				"tool_call_id": "call_123",
				"content":      "file contents here",
			},
		},
	}

	SanitizePayload(deps, payload, "nvidia")

	msgs := payload["messages"].([]interface{})
	assistant := msgs[0].(map[string]interface{})
	tc, ok := assistant["tool_calls"].([]interface{})
	if !ok || len(tc) != 1 {
		t.Fatal("tool_calls should be preserved")
	}
	if tc[0].(map[string]interface{})["id"] != "call_123" {
		t.Errorf("tool_call id mismatch: %v", tc[0].(map[string]interface{})["id"])
	}
	fn := tc[0].(map[string]interface{})["function"].(map[string]interface{})
	if fn["name"] != "read_file" {
		t.Errorf("function name mismatch: %v", fn["name"])
	}

	toolMsg := msgs[1].(map[string]interface{})
	if toolMsg["tool_call_id"] != "call_123" {
		t.Errorf("tool_call_id mismatch: %v", toolMsg["tool_call_id"])
	}
	if toolMsg["role"] != "tool" {
		t.Errorf("role mismatch: %v", toolMsg["role"])
	}
}

func TestSanitizePayload_ReasoningParamsPassthrough(t *testing.T) {
	providers := map[string]*config.Provider{
		"nvidia": {Name: "nvidia"},
	}
	deps := defaultSanitizeDeps()
	deps.Providers = providers

	payload := map[string]interface{}{
		"model":                 "nemotron-3-super",
		"reasoning_effort":      "high",
		"max_completion_tokens": 16384,
		"temperature":           0.7,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "think about this"},
		},
	}

	SanitizePayload(deps, payload, "nvidia")

	if payload["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort should pass through, got %v", payload["reasoning_effort"])
	}
	if payload["max_completion_tokens"] != 16384 {
		t.Errorf("max_completion_tokens should pass through, got %v", payload["max_completion_tokens"])
	}
	if payload["temperature"] != 0.7 {
		t.Errorf("temperature should pass through, got %v", payload["temperature"])
	}
}
