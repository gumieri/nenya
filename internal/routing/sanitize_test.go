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
		Catalog: discovery.NewModelCatalog(),
	}
}

func TestSanitizePayload_StripStreamOptions(t *testing.T) {
	deps := defaultSanitizeDeps()

	payload := map[string]interface{}{
		"model": "nemotron-3-super",
		"stream_options": map[string]interface{}{
			"include_usage": true,
		},
	}
	SanitizePayload(deps, payload, "nemotron-3-super")
	if _, ok := payload["stream_options"]; ok {
		t.Fatal("stream_options should be stripped for nemotron-3-super (inference returns no capability)")
	}

	payload2 := map[string]interface{}{
		"model": "qwen/qwen3-32b",
		"stream_options": map[string]interface{}{
			"include_usage": true,
		},
	}
	SanitizePayload(deps, payload2, "qwen/qwen3-32b")
	if _, ok := payload2["stream_options"]; !ok {
		t.Fatal("stream_options should be preserved for qwen3 (inference returns capability)")
	}
}

func TestSanitizePayload_StripToolChoiceAuto(t *testing.T) {
	deps := defaultSanitizeDeps()

	payload := map[string]interface{}{
		"model": "nemotron-3-super",
		"tool_choice": "auto",
	}
	SanitizePayload(deps, payload, "nemotron-3-super")
	if _, ok := payload["tool_choice"]; ok {
		t.Fatal("tool_choice auto should be stripped for nemotron-3-super")
	}

	payload2 := map[string]interface{}{
		"model": "gpt-4o",
		"tool_choice": "auto",
	}
	SanitizePayload(deps, payload2, "gpt-4o")
	if tc, ok := payload2["tool_choice"]; !ok || tc != "auto" {
		t.Fatal("tool_choice auto should be preserved for gpt-4o")
	}
}

func TestSanitizePayload_FlattenContentArrays(t *testing.T) {
	deps := defaultSanitizeDeps()

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
	SanitizePayload(deps, payload, "nemotron-3-super")
	msgs, ok := payload["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		t.Fatal("messages should exist")
	}
	msg := msgs[0].(map[string]interface{})
	content, ok := msg["content"].(string)
	if !ok || content != "hello\nworld" {
		t.Fatalf("content should be flattened, got %v", msg["content"])
	}

	payload2 := map[string]interface{}{
		"model": "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "hello"},
				},
			},
		},
	}
	SanitizePayload(deps, payload2, "gpt-4o")
	msgs2, ok := payload2["messages"].([]interface{})
	if !ok || len(msgs2) == 0 {
		t.Fatal("messages should exist")
	}
	msg2 := msgs2[0].(map[string]interface{})
	if _, ok := msg2["content"].([]interface{}); !ok {
		t.Fatal("content array should be preserved for gpt-4o")
	}
}

func TestSanitizePayload_StripReasoningContent(t *testing.T) {
	deps := defaultSanitizeDeps()

	payload := map[string]interface{}{
		"model": "nemotron-3-super",
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"content": "hello",
				"reasoning_content": "thinking...",
			},
		},
	}
	SanitizePayload(deps, payload, "nemotron-3-super")
	msgs, ok := payload["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		t.Fatal("messages should exist")
	}
	msg := msgs[0].(map[string]interface{})
	if _, hasReasoning := msg["reasoning_content"]; hasReasoning {
		t.Fatal("reasoning_content should be stripped for non-reasoning models")
	}

	payload2 := map[string]interface{}{
		"model": "deepseek-v4-pro",
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"content": "hello",
				"reasoning_content": "thinking...",
			},
		},
	}
	SanitizePayload(deps, payload2, "deepseek-v4-pro")
	msgs2, ok := payload2["messages"].([]interface{})
	if !ok || len(msgs2) == 0 {
		t.Fatal("messages should exist")
	}
	msg2 := msgs2[0].(map[string]interface{})
	if _, hasReasoning := msg2["reasoning_content"]; !hasReasoning {
		t.Fatal("reasoning_content should be preserved for deepseek-v4-pro")
	}
}

func TestSanitizePayload_DeepSeekReasoningInjection(t *testing.T) {
	deps := defaultSanitizeDeps()

	payload := map[string]interface{}{
		"model": "deepseek-v4-pro",
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"content": "hello",
			},
		},
	}
	SanitizePayload(deps, payload, "deepseek-v4-pro")
	msgs, ok := payload["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		t.Fatal("messages should exist")
	}
	msg := msgs[0].(map[string]interface{})
	rc, ok := msg["reasoning_content"].(string)
	if !ok || rc != "" {
		t.Fatalf("reasoning_content should be injected as empty string, got %v", rc)
	}
}

func TestSanitizePayload_DeepSeekStripThinkingParams(t *testing.T) {
	deps := defaultSanitizeDeps()

	payload := map[string]interface{}{
		"model": "deepseek-v4-pro",
		"thinking": map[string]interface{}{
			"type": "enabled",
		},
		"temperature": 0.7,
		"top_p": 0.9,
	}
	SanitizePayload(deps, payload, "deepseek-v4-pro")
	if _, has := payload["temperature"]; has {
		t.Fatal("temperature should be stripped for deepseek with thinking enabled")
	}
	if _, has := payload["top_p"]; has {
		t.Fatal("top_p should be stripped for deepseek with thinking enabled")
	}
}

func TestSanitizePayload_RepairMessageOrdering(t *testing.T) {
	deps := defaultSanitizeDeps()

	payload := map[string]interface{}{
		"model": "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": "call tool",
			},
			map[string]interface{}{
				"role": "assistant",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":   "123",
						"type": "function",
						"function": map[string]interface{}{
							"name": "test",
						},
					},
				},
			},
			map[string]interface{}{
				"role":    "tool",
				"content": "result",
			},
			map[string]interface{}{
				"role":    "user",
				"content": "continue",
			},
		},
	}
	SanitizePayload(deps, payload, "gpt-4o")
	msgs, ok := payload["messages"].([]interface{})
	if !ok {
		t.Fatal("messages should exist")
	}
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages after repair, got %d", len(msgs))
	}
	if msgs[3].(map[string]interface{})["role"] != "assistant" {
		t.Fatal("expected assistant bridge message inserted at index 3")
	}
}

func TestSanitizePayload_NoOpOnValidOrdering(t *testing.T) {
	deps := defaultSanitizeDeps()

	payload := map[string]interface{}{
		"model": "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": "call tool",
			},
			map[string]interface{}{
				"role": "assistant",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":   "123",
						"type": "function",
						"function": map[string]interface{}{
							"name": "test",
						},
					},
				},
			},
			map[string]interface{}{
				"role":    "tool",
				"content": "result",
			},
			map[string]interface{}{
				"role": "assistant",
				"content": "done",
			},
			map[string]interface{}{
				"role":    "user",
				"content": "continue",
			},
		},
	}
	originalLen := len(payload["messages"].([]interface{}))
	SanitizePayload(deps, payload, "gpt-4o")
	newLen := len(payload["messages"].([]interface{}))
	if newLen != originalLen {
		t.Fatalf("message count should not change for valid ordering: %d -> %d", originalLen, newLen)
	}
}

func TestSanitizePayload_NonDeepSeekNoReasoningInjection(t *testing.T) {
	deps := defaultSanitizeDeps()

	payload := map[string]interface{}{
		"model": "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"content": "hello",
			},
		},
	}
	SanitizePayload(deps, payload, "gpt-4o")
	msgs, ok := payload["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		t.Fatal("messages should exist")
	}
	msg := msgs[0].(map[string]interface{})
	if _, hasReasoning := msg["reasoning_content"]; hasReasoning {
		t.Fatal("reasoning_content should not be injected for non-deepseek models")
	}
}

func TestSanitizePayload_DeepSeekDoesNotStripReasoning(t *testing.T) {
	deps := defaultSanitizeDeps()

	payload := map[string]interface{}{
		"model": "deepseek-v4-pro",
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"content": "hello",
				"reasoning_content": "thinking...",
			},
		},
	}
	SanitizePayload(deps, payload, "deepseek-v4-pro")
	msgs, ok := payload["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		t.Fatal("messages should exist")
	}
	msg := msgs[0].(map[string]interface{})
	if rc, ok := msg["reasoning_content"].(string); !ok || rc != "thinking..." {
		t.Fatalf("reasoning_content should be preserved for deepseek, got %v", rc)
	}
}

func TestSanitizePayload_NonDeepSeekWithThinkingStripsNothing(t *testing.T) {
	deps := defaultSanitizeDeps()

	payload := map[string]interface{}{
		"model": "gpt-4o",
		"thinking": map[string]interface{}{
			"type": "enabled",
		},
		"temperature": 0.7,
	}
	SanitizePayload(deps, payload, "gpt-4o")
	if _, has := payload["temperature"]; !has {
		t.Fatal("temperature should be preserved for non-deepseek models with thinking")
	}
}

func TestSanitizePayload_DeepSeekFlashReasoningInjection(t *testing.T) {
	deps := defaultSanitizeDeps()

	payload := map[string]interface{}{
		"model": "deepseek-v4-flash",
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"content": "quick response",
			},
		},
	}
	SanitizePayload(deps, payload, "deepseek-v4-flash")
	msgs, ok := payload["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		t.Fatal("messages should exist")
	}
	msg := msgs[0].(map[string]interface{})
	rc, ok := msg["reasoning_content"].(string)
	if !ok || rc != "" {
		t.Fatalf("reasoning_content should be injected for deepseek-v4-flash, got %v", rc)
	}
}