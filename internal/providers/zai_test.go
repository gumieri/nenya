package providers

import (
	"log/slog"
	"os"
	"testing"
)

func zaiDeps() *SanitizeDeps {
	return &SanitizeDeps{
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
		ExtractContentText: func(msg map[string]interface{}) string {
			if c, ok := msg["content"].(string); ok {
				return c
			}
			return ""
		},
	}
}

func TestZAI_MergesConsecutiveUserMessages(t *testing.T) {
	deps := zaiDeps()

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "first part"},
			map[string]interface{}{"role": "user", "content": "second part"},
			map[string]interface{}{"role": "assistant", "content": "response"},
		},
	}

	zaiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (1 system bridge + 1 merged user + 1 assistant), got %d", len(msgs))
	}

	if msgs[0].(map[string]interface{})["role"] != "system" {
		t.Error("expected system bridge as first message")
	}

	user := msgs[1].(map[string]interface{})
	if user["content"] != "first part\n\nsecond part" {
		t.Errorf("expected merged user content, got %q", user["content"])
	}
}

func TestZAI_PrependsSystemBridge(t *testing.T) {
	deps := zaiDeps()

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
			map[string]interface{}{"role": "assistant", "content": "hi"},
		},
	}

	zaiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	if len(msgs) < 1 {
		t.Fatal("expected at least 1 message")
	}
	if msgs[0].(map[string]interface{})["role"] != "system" {
		t.Error("expected system bridge prepended")
	}
	if msgs[0].(map[string]interface{})["content"] != "Continue the conversation." {
		t.Errorf("expected 'Continue the conversation.', got %q", msgs[0].(map[string]interface{})["content"])
	}
}

func TestZAI_NoSystemBridgeWhenSystemFirst(t *testing.T) {
	deps := zaiDeps()

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "You are helpful."},
			map[string]interface{}{"role": "user", "content": "hello"},
			map[string]interface{}{"role": "assistant", "content": "hi"},
		},
	}

	zaiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	if len(msgs) < 1 {
		t.Fatal("expected at least 1 message")
	}
	if msgs[0].(map[string]interface{})["role"] == "system" &&
		msgs[0].(map[string]interface{})["content"] == "Continue the conversation." {
		t.Error("should not prepend system bridge when system message is already first")
	}
}

func TestZAI_InsertsUserBridgeBetweenConsecutiveAssistants(t *testing.T) {
	deps := zaiDeps()

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "assistant", "content": "first response"},
			map[string]interface{}{"role": "assistant", "content": "second response"},
		},
	}

	zaiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	foundBridge := false
	for _, m := range msgs {
		msg := m.(map[string]interface{})
		if msg["role"] == "user" && msg["content"] == "Continue." {
			foundBridge = true
			break
		}
	}
	if !foundBridge {
		t.Error("expected user bridge 'Continue.' between consecutive assistant messages")
	}
}

func TestZAI_RemovesOrphanedToolMessages(t *testing.T) {
	deps := zaiDeps()

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
			map[string]interface{}{
				"role": "assistant",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":       "tc-1",
						"type":     "function",
						"function": map[string]interface{}{"name": "read_file"},
					},
				},
			},
			map[string]interface{}{
				"role":         "tool",
				"tool_call_id": "tc-1",
				"content":      "file contents",
			},
			map[string]interface{}{
				"role":         "tool",
				"tool_call_id": "tc-orphan",
				"content":      "orphan response",
			},
		},
	}

	zaiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	for _, m := range msgs {
		msg := m.(map[string]interface{})
		if msg["role"] == "tool" && msg["tool_call_id"] == "tc-orphan" {
			t.Error("orphaned tool message should have been removed")
		}
	}
}

func TestZAI_RemovesToolMessagesWithoutToolCallID(t *testing.T) {
	deps := zaiDeps()

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
			map[string]interface{}{
				"role":    "tool",
				"content": "no tool_call_id here",
			},
		},
	}

	zaiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	for _, m := range msgs {
		msg := m.(map[string]interface{})
		if msg["role"] == "tool" {
			t.Error("tool message without tool_call_id should have been removed")
		}
	}
}

func TestZAI_RemovesEmptyMessages(t *testing.T) {
	deps := zaiDeps()

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": ""},
			map[string]interface{}{"role": "assistant", "content": "response"},
		},
	}

	zaiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	for _, m := range msgs {
		msg := m.(map[string]interface{})
		if msg["role"] == "user" && msg["content"] == "" {
			t.Error("empty user message should have been removed")
		}
	}
}

func TestZAI_KeepsEmptyAssistantWithToolCalls(t *testing.T) {
	deps := zaiDeps()

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":       "tc-1",
						"type":     "function",
						"function": map[string]interface{}{"name": "do_thing"},
					},
				},
			},
			map[string]interface{}{
				"role":         "tool",
				"tool_call_id": "tc-1",
				"content":      "result",
			},
		},
	}

	zaiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	found := false
	for _, m := range msgs {
		msg := m.(map[string]interface{})
		if msg["role"] == "assistant" {
			found = true
			break
		}
	}
	if !found {
		t.Error("empty assistant with tool_calls should be kept")
	}
}

func TestZAI_NoMessages(t *testing.T) {
	deps := zaiDeps()

	payload := map[string]interface{}{
		"messages": []interface{}{},
	}

	zaiSanitize(deps, payload)

	if _, ok := payload["messages"].([]interface{}); !ok {
		t.Error("messages should still be present")
	}
}

func TestZAI_SkipsSanitizeWhenToolsPresent(t *testing.T) {
	deps := zaiDeps()

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "first part"},
			map[string]interface{}{"role": "user", "content": "second part"},
			map[string]interface{}{"role": "assistant", "content": "response"},
		},
		"tools": []interface{}{
			map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "read_file"}},
		},
	}

	zaiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
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
}

func TestZAI_SkipsSanitizeWhenToolChoicePresent(t *testing.T) {
	deps := zaiDeps()

	origMessages := []interface{}{
		map[string]interface{}{"role": "user", "content": "first part"},
		map[string]interface{}{"role": "user", "content": "second part"},
	}

	payload := map[string]interface{}{
		"messages":    origMessages,
		"tool_choice": "auto",
	}

	zaiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (untouched), got %d", len(msgs))
	}
}

func TestZAI_SkipsSanitizeWhenToolsAndToolCalls(t *testing.T) {
	deps := zaiDeps()

	origMessages := []interface{}{
		map[string]interface{}{
			"role": "assistant",
			"tool_calls": []interface{}{
				map[string]interface{}{
					"id":   "tc-1",
					"type": "function",
					"function": map[string]interface{}{
						"name":      "mcp__search",
						"arguments": `{"query":"hello"}`,
					},
				},
			},
		},
		map[string]interface{}{
			"role":         "tool",
			"tool_call_id": "tc-1",
			"content":      "result",
		},
		map[string]interface{}{
			"role":         "tool",
			"tool_call_id": "tc-orphan",
			"content":      "orphan result",
		},
		map[string]interface{}{
			"role":    "user",
			"content": "",
		},
	}

	payload := map[string]interface{}{
		"messages": origMessages,
		"tools":    []interface{}{},
	}

	zaiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages (untouched), got %d", len(msgs))
	}
	for _, m := range msgs {
		msg := m.(map[string]interface{})
		if msg["role"] == "tool" && msg["tool_call_id"] == "tc-orphan" {
			return
		}
	}
	t.Error("orphaned tool message should still be present when tools guard is active")
}

func TestZAI_SanitizesWhenNoTools(t *testing.T) {
	deps := zaiDeps()

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "first part"},
			map[string]interface{}{"role": "user", "content": "second part"},
			map[string]interface{}{"role": "assistant", "content": "response"},
		},
	}

	zaiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (1 system bridge + 1 merged user + 1 assistant), got %d", len(msgs))
	}
	if msgs[0].(map[string]interface{})["role"] != "system" {
		t.Error("expected system bridge as first message when no tools present")
	}
}

func TestZAI_InjectThinkingForReasoningModel(t *testing.T) {
	deps := zaiDeps()
	deps.SupportsReasoning = func(model string) bool {
		return model == "glm-5"
	}

	payload := map[string]interface{}{
		"model": "glm-5",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}

	zaiSanitize(deps, payload)

	thinking, ok := payload["thinking"].(map[string]interface{})
	if !ok {
		t.Fatal("expected thinking to be injected")
	}
	if thinking["type"] != "enabled" {
		t.Fatalf("expected thinking.type to be enabled, got %v", thinking["type"])
	}
}

func TestZAI_SkipThinkingForNonReasoningModel(t *testing.T) {
	deps := zaiDeps()
	deps.SupportsReasoning = func(model string) bool {
		return false
	}

	payload := map[string]interface{}{
		"model": "glm-4",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}

	zaiSanitize(deps, payload)

	if _, hasThinking := payload["thinking"]; hasThinking {
		t.Fatal("expected thinking NOT to be injected for non-reasoning model")
	}
}

func TestZAI_ForceEnableThinking(t *testing.T) {
	deps := zaiDeps()
	deps.SupportsReasoning = func(model string) bool {
		return false
	}
	deps.ProviderThinking = func(name string) (bool, bool, bool) {
		if name == "zai" {
			return true, false, true
		}
		return false, false, false
	}

	payload := map[string]interface{}{
		"model": "glm-4",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}

	zaiSanitize(deps, payload)

	thinking, ok := payload["thinking"].(map[string]interface{})
	if !ok {
		t.Fatal("expected thinking to be injected despite non-reasoning model")
	}
	if thinking["type"] != "enabled" {
		t.Fatalf("expected thinking.type enabled, got %v", thinking["type"])
	}
}

func TestZAI_ForceDisableThinking(t *testing.T) {
	deps := zaiDeps()
	deps.SupportsReasoning = func(model string) bool {
		return true
	}
	deps.ProviderThinking = func(name string) (bool, bool, bool) {
		if name == "zai" {
			return false, false, true
		}
		return false, false, false
	}

	payload := map[string]interface{}{
		"model": "glm-5",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}

	zaiSanitize(deps, payload)

	if _, hasThinking := payload["thinking"]; hasThinking {
		t.Fatal("expected thinking NOT to be injected when force-disabled")
	}
}

func TestZAI_RespectsClientProvidedThinking(t *testing.T) {
	deps := zaiDeps()
	deps.SupportsReasoning = func(model string) bool {
		return true
	}

	payload := map[string]interface{}{
		"model": "glm-5",
		"thinking": map[string]interface{}{
			"type": "disabled",
		},
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}

	zaiSanitize(deps, payload)

	thinking := payload["thinking"].(map[string]interface{})
	if thinking["type"] != "disabled" {
		t.Fatalf("expected client's thinking type 'disabled' to be preserved, got %v", thinking["type"])
	}
}

func TestZAI_InjectTemperatureForGLM46(t *testing.T) {
	deps := zaiDeps()

	payload := map[string]interface{}{
		"model": "glm-4.6-turbo",
		"messages": []interface{}{},
	}

	zaiSanitize(deps, payload)

	temp, ok := payload["temperature"].(float64)
	if !ok || temp != 1.0 {
		t.Fatalf("expected temperature 1.0 for glm-4.6, got %v", payload["temperature"])
	}
}

func TestZAI_InjectTemperatureForGLM47(t *testing.T) {
	deps := zaiDeps()

	payload := map[string]interface{}{
		"model": "glm-4.7-plus",
		"messages": []interface{}{},
	}

	zaiSanitize(deps, payload)

	temp, ok := payload["temperature"].(float64)
	if !ok || temp != 1.0 {
		t.Fatalf("expected temperature 1.0 for glm-4.7, got %v", payload["temperature"])
	}
}

func TestZAI_NoTemperatureForOtherModels(t *testing.T) {
	deps := zaiDeps()

	payload := map[string]interface{}{
		"model": "glm-5",
		"messages": []interface{}{},
	}

	zaiSanitize(deps, payload)

	if _, hasTemp := payload["temperature"]; hasTemp {
		t.Fatal("expected no temperature injection for non-glm-4.x models")
	}
}

func TestZAI_PreservesClientTemperature(t *testing.T) {
	deps := zaiDeps()

	payload := map[string]interface{}{
		"model":       "glm-4.6-turbo",
		"temperature": 0.5,
		"messages":    []interface{}{},
	}

	zaiSanitize(deps, payload)

	temp := payload["temperature"].(float64)
	if temp != 0.5 {
		t.Fatalf("expected client temperature 0.5 preserved, got %v", temp)
	}
}
