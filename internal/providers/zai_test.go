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
