package routing

import (
	"log/slog"
	"os"
	"testing"

	"nenya/config"
	"nenya/internal/discovery"
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
		Catalog: nil,
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
	SanitizePayload(deps, payload, "nvidia", "nemotron-3-super")
	if _, ok := payload["stream_options"]; ok {
		t.Fatal("stream_options should be stripped for nvidia")
	}

	payload2 := map[string]interface{}{
		"model": "gemini-2.5-flash",
		"stream_options": map[string]interface{}{
			"include_usage": true,
		},
	}
	SanitizePayload(deps, payload2, "gemini", "gemini-2.5-flash")
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
		"model": "deepseek-v4-pro",
		"stream_options": map[string]interface{}{
			"include_usage": true,
		},
	}
	SanitizePayload(deps, payload, "deepseek", "deepseek-v4-pro")
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
	SanitizePayload(deps, payload, "nvidia", "nemotron-3-super")
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
		"model":       "deepseek-v4-pro",
		"tool_choice": "auto",
	}
	SanitizePayload(deps, payload, "deepseek", "deepseek-v4-pro")
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
	SanitizePayload(deps, payload, "nvidia", "nemotron-3-super")
	if _, ok := payload["tool_choice"]; !ok {
		t.Fatal("non-string tool_choice (object) should be kept")
	}
}

func TestSanitizePayload_FlattenContentArrays(t *testing.T) {
	providers := map[string]*config.Provider{
		"ollama": {Name: "ollama"},
	}
	deps := defaultSanitizeDeps()
	deps.Providers = providers

	payload := map[string]interface{}{
		"model": "qwen2.5-coder:7b",
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
	SanitizePayload(deps, payload, "ollama", "qwen2.5-coder:7b")
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
		"model":    "deepseek-v4-pro",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": contentArr}},
	}
	SanitizePayload(deps, payload, "deepseek", "deepseek-v4-pro")
	msgs := payload["messages"].([]interface{})
	arr, ok := msgs[0].(map[string]interface{})["content"].([]interface{})
	if !ok || len(arr) != 1 {
		t.Fatal("content array should be kept for deepseek")
	}
}

func TestSanitizePayload_NoMessages(t *testing.T) {
	providers := map[string]*config.Provider{
		"ollama": {Name: "ollama"},
	}
	deps := defaultSanitizeDeps()
	deps.Providers = providers

	payload := map[string]interface{}{
		"model": "qwen2.5-coder:7b",
	}

	SanitizePayload(deps, payload, "nvidia", "qwen2.5-coder:7b")
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

	SanitizePayload(deps, payload, "nvidia", "nemotron-3-super")

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

	SanitizePayload(deps, payload, "nvidia", "nemotron-3-super")

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

func TestSanitizePayload_StripReasoningContentForNonReasoningProvider(t *testing.T) {
	deps := defaultSanitizeDeps()
	deps.Providers = map[string]*config.Provider{
		"nvidia": {Name: "nvidia"},
	}

	payload := map[string]interface{}{
		"model": "nemotron-3-super",
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": "hello",
			},
			map[string]interface{}{
				"role":              "assistant",
				"content":           "answer",
				"reasoning_content": "some reasoning that nvidia does not support",
			},
		},
	}

	SanitizePayload(deps, payload, "nvidia", "nemotron-3-super")
	msgs := payload["messages"].([]interface{})
	assistant := msgs[1].(map[string]interface{})
	if _, exists := assistant["reasoning_content"]; exists {
		t.Fatal("reasoning_content should be stripped for non-reasoning provider")
	}
}

func TestSanitizePayload_KeepReasoningContentForReasoningProvider(t *testing.T) {
	deps := defaultSanitizeDeps()
	deps.Providers = map[string]*config.Provider{
		"deepseek": {Name: "deepseek"},
	}

	payload := map[string]interface{}{
		"model": "deepseek-v4-pro",
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": "hello",
			},
			map[string]interface{}{
				"role":              "assistant",
				"content":           "answer",
				"reasoning_content": "required reasoning for deepseek v4 thinking mode",
			},
		},
	}

	SanitizePayload(deps, payload, "deepseek", "deepseek-v4-pro")
	msgs := payload["messages"].([]interface{})
	assistant := msgs[1].(map[string]interface{})
	if _, exists := assistant["reasoning_content"]; !exists {
		t.Fatal("reasoning_content should be preserved for reasoning provider")
	}
	if assistant["reasoning_content"].(string) != "required reasoning for deepseek v4 thinking mode" {
		t.Fatalf("reasoning_content value mismatch: %v", assistant["reasoning_content"])
	}
}

func TestSanitizePayload_KeepEmptyReasoningContent(t *testing.T) {
	deps := defaultSanitizeDeps()
	deps.Providers = map[string]*config.Provider{
		"nvidia": {Name: "nvidia"},
	}

	payload := map[string]interface{}{
		"model": "nemotron-3-super",
		"messages": []interface{}{
			map[string]interface{}{
				"role":              "assistant",
				"content":           "answer",
				"reasoning_content": "",
			},
		},
	}

	SanitizePayload(deps, payload, "nvidia", "nemotron-3-super")
	msgs := payload["messages"].([]interface{})
	assistant := msgs[0].(map[string]interface{})
	if _, exists := assistant["reasoning_content"]; !exists {
		t.Fatal("empty reasoning_content should remain (no-op, nothing to strip)")
	}
}

func TestSanitizePayload_StripDeepSeekThinkingParams(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		thinking interface{}
		extra    map[string]interface{}
		want     []string
	}{
		{
			name:     "enabled strips all four",
			provider: "deepseek",
			thinking: map[string]interface{}{"type": "enabled"},
			extra:    map[string]interface{}{"temperature": 0.7, "top_p": 0.9, "presence_penalty": 1.0, "frequency_penalty": 1.0},
			want:     []string{"temperature", "top_p", "presence_penalty", "frequency_penalty"},
		},
		{
			name:     "disabled keeps params",
			provider: "deepseek",
			thinking: map[string]interface{}{"type": "disabled"},
			extra:    map[string]interface{}{"temperature": 0.7},
			want:     nil,
		},
		{
			name:     "no thinking keeps params",
			provider: "deepseek",
			thinking: nil,
			extra:    map[string]interface{}{"temperature": 0.7},
			want:     nil,
		},
		{
			name:     "non-deepseek keeps params",
			provider: "anthropic",
			thinking: map[string]interface{}{"type": "enabled"},
			extra:    map[string]interface{}{"temperature": 0.7},
			want:     nil,
		},
		{
			name:     "string thinking value keeps params",
			provider: "deepseek",
			thinking: "enabled",
			extra:    map[string]interface{}{"temperature": 0.7},
			want:     nil,
		},
		{
			name:     "missing type key keeps params",
			provider: "deepseek",
			thinking: map[string]interface{}{"budget": 100},
			extra:    map[string]interface{}{"temperature": 0.7},
			want:     nil,
		},
		{
			name:     "no extra params no-op",
			provider: "deepseek",
			thinking: map[string]interface{}{"type": "enabled"},
			extra:    nil,
			want:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := defaultSanitizeDeps()
			deps.Providers = map[string]*config.Provider{
				"deepseek": {Name: "deepseek"},
			}
			payload := map[string]interface{}{
				"model":    "deepseek-v4-pro",
				"messages": []interface{}{},
			}
			if tt.thinking != nil {
				payload["thinking"] = tt.thinking
			}
			for k, v := range tt.extra {
				payload[k] = v
			}
			SanitizePayload(deps, payload, tt.provider, "deepseek-v4-pro")
			for _, key := range tt.want {
				if _, exists := payload[key]; exists {
					t.Errorf("expected %q to be stripped, but it remains", key)
				}
			}
			if len(tt.want) == 0 {
				for k := range tt.extra {
					if _, exists := payload[k]; !exists {
						t.Errorf("expected %q to be preserved, but it was stripped", k)
					}
				}
			}
		})
	}
}

func TestSanitizePayload_StripReasoningForNonReasoningModel(t *testing.T) {
	catalog := discovery.NewModelCatalog()
	catalog.Add(discovery.DiscoveredModel{
		ID:       "qwen/qwen3-32b",
		Provider: "groq",
		Metadata: &discovery.ModelMetadata{},
	})

	deps := defaultSanitizeDeps()
	deps.Catalog = catalog

	payload := map[string]interface{}{
		"model": "qwen/qwen3-32b",
		"messages": []interface{}{
			map[string]interface{}{
				"role":              "assistant",
				"content":           "answer",
				"reasoning_content": "thoughts from a reasoning model",
			},
		},
	}

	SanitizePayload(deps, payload, "groq", "qwen/qwen3-32b")
	msgs := payload["messages"].([]interface{})
	assistant := msgs[0].(map[string]interface{})
	if _, exists := assistant["reasoning_content"]; exists {
		t.Fatal("reasoning_content should be stripped for non-reasoning model on reasoning provider")
	}
}

func TestSanitizePayload_KeepReasoningForReasoningModel(t *testing.T) {
	catalog := discovery.NewModelCatalog()
	catalog.Add(discovery.DiscoveredModel{
		ID:       "deepseek-r1",
		Provider: "groq",
		Metadata: &discovery.ModelMetadata{
			SupportsReasoning: true,
		},
	})

	deps := defaultSanitizeDeps()
	deps.Catalog = catalog

	payload := map[string]interface{}{
		"model": "deepseek-r1",
		"messages": []interface{}{
			map[string]interface{}{
				"role":              "assistant",
				"content":           "answer",
				"reasoning_content": "required reasoning",
			},
		},
	}

	SanitizePayload(deps, payload, "groq", "deepseek-r1")
	msgs := payload["messages"].([]interface{})
	assistant := msgs[0].(map[string]interface{})
	if _, exists := assistant["reasoning_content"]; !exists {
		t.Fatal("reasoning_content should be preserved for reasoning model on reasoning provider")
	}
}

func TestSanitizePayload_KeepReasoningForUnknownModelOnReasoningProvider(t *testing.T) {
	catalog := discovery.NewModelCatalog()

	deps := defaultSanitizeDeps()
	deps.Catalog = catalog

	payload := map[string]interface{}{
		"model": "deepseek-v4-flash",
		"messages": []interface{}{
			map[string]interface{}{
				"role":              "assistant",
				"content":           "answer",
				"reasoning_content": "required reasoning",
			},
		},
	}

	SanitizePayload(deps, payload, "deepseek", "deepseek-v4-flash")
	msgs := payload["messages"].([]interface{})
	assistant := msgs[0].(map[string]interface{})
	if _, exists := assistant["reasoning_content"]; !exists {
		t.Fatal("reasoning_content should be preserved when model is missing from catalog but provider supports reasoning")
	}
}

func TestSanitizePayload_InjectReasoningContentForDeepSeek(t *testing.T) {
	deps := defaultSanitizeDeps()
	deps.Providers = map[string]*config.Provider{
		"deepseek": {Name: "deepseek"},
	}

	payload := map[string]interface{}{
		"model": "deepseek-v4-pro",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
			map[string]interface{}{"role": "assistant", "content": "answer"},
			map[string]interface{}{"role": "user", "content": "followup"},
		},
	}

	SanitizePayload(deps, payload, "deepseek", "deepseek-v4-pro")
	msgs := payload["messages"].([]interface{})
	assistant := msgs[1].(map[string]interface{})
	rc, ok := assistant["reasoning_content"].(string)
	if !ok || rc != "" {
		t.Fatalf("expected empty reasoning_content injected, got %v", assistant["reasoning_content"])
	}
}

func TestSanitizePayload_InjectReasoningOnBridgeMessage(t *testing.T) {
	deps := defaultSanitizeDeps()
	deps.Providers = map[string]*config.Provider{
		"deepseek": {Name: "deepseek"},
	}

	payload := map[string]interface{}{
		"model": "deepseek-v4-pro",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
			map[string]interface{}{"role": "assistant", "content": "", "tool_calls": []interface{}{map[string]interface{}{"id": "c1"}}},
			map[string]interface{}{"role": "tool", "tool_call_id": "c1", "content": "result"},
			map[string]interface{}{"role": "user", "content": "next question"},
		},
	}

	SanitizePayload(deps, payload, "deepseek", "deepseek-v4-pro")
	msgs := payload["messages"].([]interface{})
	// After repair: [user, assistant(tool_call), tool, bridge, user]
	// Bridge is at index 3
	if len(msgs) < 4 {
		t.Fatalf("expected at least 4 messages after repair, got %d", len(msgs))
	}
	bridge := msgs[3].(map[string]interface{})
	if bridge["role"] != "assistant" {
		t.Fatalf("expected bridge at index 3 with role assistant, got role %v", bridge["role"])
	}
	rc, ok := bridge["reasoning_content"].(string)
	if !ok || rc != "" {
		t.Fatalf("expected empty reasoning_content on bridge message, got %v", bridge["reasoning_content"])
	}
}

func TestSanitizePayload_SkipStripReasoningForDeepSeek(t *testing.T) {
	catalog := discovery.NewModelCatalog()
	catalog.Add(discovery.DiscoveredModel{
		ID:       "deepseek-v4-pro",
		Provider: "deepseek",
		Metadata: &discovery.ModelMetadata{},
	})

	deps := defaultSanitizeDeps()
	deps.Catalog = catalog
	deps.Providers = map[string]*config.Provider{
		"deepseek": {Name: "deepseek"},
	}

	payload := map[string]interface{}{
		"model": "deepseek-v4-pro",
		"messages": []interface{}{
			map[string]interface{}{
				"role":              "assistant",
				"content":           "answer",
				"reasoning_content": "should not be stripped",
			},
		},
	}

	SanitizePayload(deps, payload, "deepseek", "deepseek-v4-pro")
	msgs := payload["messages"].([]interface{})
	assistant := msgs[0].(map[string]interface{})
	rc, ok := assistant["reasoning_content"].(string)
	if !ok || rc != "should not be stripped" {
		t.Fatalf("reasoning_content should be preserved for deepseek even when catalog says no reasoning, got %v", assistant["reasoning_content"])
	}
}

func TestRepairMessageOrdering(t *testing.T) {
	tests := []struct {
		name       string
		messages   []interface{}
		wantRepair bool
		wantLen    int
	}{
		{
			name: "valid sequence no repair",
			messages: []interface{}{
				map[string]interface{}{"role": "user", "content": "hello"},
				map[string]interface{}{"role": "assistant", "content": "hi"},
			},
			wantRepair: false,
			wantLen:    2,
		},
		{
			name: "tool then user needs bridge",
			messages: []interface{}{
				map[string]interface{}{"role": "user", "content": "hello"},
				map[string]interface{}{"role": "assistant", "content": "", "tool_calls": []interface{}{}},
				map[string]interface{}{"role": "tool", "tool_call_id": "c1", "content": "result"},
				map[string]interface{}{"role": "user", "content": "next question"},
			},
			wantRepair: true,
			wantLen:    5,
		},
		{
			name: "multiple tool then user",
			messages: []interface{}{
				map[string]interface{}{"role": "user", "content": "hello"},
				map[string]interface{}{"role": "assistant", "content": "", "tool_calls": []interface{}{}},
				map[string]interface{}{"role": "tool", "tool_call_id": "c1", "content": "r1"},
				map[string]interface{}{"role": "tool", "tool_call_id": "c2", "content": "r2"},
				map[string]interface{}{"role": "user", "content": "next"},
			},
			wantRepair: true,
			wantLen:    6,
		},
		{
			name: "tool then assistant no repair",
			messages: []interface{}{
				map[string]interface{}{"role": "user", "content": "hello"},
				map[string]interface{}{"role": "assistant", "content": "", "tool_calls": []interface{}{}},
				map[string]interface{}{"role": "tool", "tool_call_id": "c1", "content": "result"},
				map[string]interface{}{"role": "assistant", "content": "summary"},
			},
			wantRepair: false,
			wantLen:    4,
		},
		{
			name: "empty messages",
			messages: []interface{}{
				map[string]interface{}{"role": "assistant", "content": "", "tool_calls": []interface{}{}},
				map[string]interface{}{"role": "tool", "tool_call_id": "c1", "content": "result"},
				map[string]interface{}{"role": "user", "content": "next"},
			},
			wantRepair: true,
			wantLen:    4,
		},
		{
			name:       "single message no repair",
			messages:   []interface{}{map[string]interface{}{"role": "user", "content": "hello"}},
			wantRepair: false,
			wantLen:    1,
		},
		{
			name:       "empty slice no repair",
			messages:   []interface{}{},
			wantRepair: false,
			wantLen:    0,
		},
		{
			name: "double tool-user repair",
			messages: []interface{}{
				map[string]interface{}{"role": "assistant", "content": "", "tool_calls": []interface{}{}},
				map[string]interface{}{"role": "tool", "tool_call_id": "c1", "content": "r1"},
				map[string]interface{}{"role": "user", "content": "followup1"},
				map[string]interface{}{"role": "assistant", "content": "", "tool_calls": []interface{}{}},
				map[string]interface{}{"role": "tool", "tool_call_id": "c2", "content": "r2"},
				map[string]interface{}{"role": "user", "content": "followup2"},
			},
			wantRepair: true,
			wantLen:    8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repaired, repairedMessages := repairMessageOrdering(tt.messages)
			if repaired != tt.wantRepair {
				t.Errorf("repaired = %v, want %v", repaired, tt.wantRepair)
			}
			if len(repairedMessages) != tt.wantLen {
				t.Errorf("len(repairedMessages) = %d, want %d", len(repairedMessages), tt.wantLen)
			}
		})
	}
}
