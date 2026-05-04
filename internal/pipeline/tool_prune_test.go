package pipeline

import (
	"strings"
	"testing"

	"nenya/config"
)

func makePayload(msgs ...interface{}) map[string]interface{} {
	return map[string]interface{}{
		"messages": msgs,
	}
}

func TestPruneStaleToolCalls_Disabled(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools: config.PtrTo(false),
	}

	payload := makePayload(
		map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{}},
	)

	if mutated := PruneStaleToolCalls(payload, cfg); mutated {
		t.Fatalf("expected false when pruning disabled, got true")
	}
}

func TestPruneStaleToolCalls_BelowProtectionWindow(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools: config.PtrTo(true),
		ToolProtectionWindow: 4,
	}

	payload := makePayload(
		map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{}},         // 0
		map[string]interface{}{"role": "user", "content": "test"},                          // 1
		map[string]interface{}{"role": "assistant", "content": "hi"},                       // 2
		map[string]interface{}{"role": "tool", "tool_call_id": "abc", "content": "result"}, // 3
	)

	if mutated := PruneStaleToolCalls(payload, cfg); mutated {
		t.Fatalf("expected false when below protection window, got true")
	}
	messages := payload["messages"].([]interface{})
	if len(messages) != 4 {
		t.Fatalf("expected length unchanged, got %d", len(messages))
	}
}

func TestPruneStaleToolCalls_SimplePair(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools: config.PtrTo(true),
		ToolProtectionWindow: 1,
	}

	payload := makePayload(
		map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{
			map[string]interface{}{
				"id": "call_1",
				"function": map[string]interface{}{
					"name": "test_tool",
				},
			},
		}}, // 0 - should be pruned (outside protection window)
		map[string]interface{}{"role": "tool", "tool_call_id": "call_1", "content": "tool result"}, // 1
		map[string]interface{}{"role": "user", "content": "current message"},                       // 2 - protected
	)

	if mutated := PruneStaleToolCalls(payload, cfg); !mutated {
		t.Fatalf("expected true, got false")
	}
	messages := payload["messages"].([]interface{})
	if len(messages) != 2 {
		t.Fatalf("expected length 2 (removed pair, added summary), got %d", len(messages))
	}

	first := messages[0].(map[string]interface{})
	if first["role"] != "assistant" {
		t.Fatalf("expected role assistant, got %v", first["role"])
	}
	second := messages[1].(map[string]interface{})
	if second["role"] != "user" {
		t.Fatalf("expected second message to be user, got %v", second["role"])
	}
	content, ok := first["content"].(string)
	if !ok {
		t.Fatalf("expected string content")
	}
	expected := "[System] Tool 'test_tool' was executed previously. Result compacted to save context window."
	if content != expected {
		t.Fatalf("expected content %q, got %q", expected, content)
	}
}

func TestPruneStaleToolCalls_MultipleCallsSameTurn(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools: config.PtrTo(true),
		ToolProtectionWindow: 1,
	}

	payload := makePayload(
		map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{
			map[string]interface{}{
				"id": "call_1",
				"function": map[string]interface{}{
					"name": "tool_one",
				},
			},
			map[string]interface{}{
				"id": "call_2",
				"function": map[string]interface{}{
					"name": "tool_two",
				},
			},
		}}, // 0 - should be pruned
		map[string]interface{}{"role": "tool", "tool_call_id": "call_1", "content": "result one"}, // 1
		map[string]interface{}{"role": "tool", "tool_call_id": "call_2", "content": "result two"}, // 2
		map[string]interface{}{"role": "user", "content": "current"},                              // 3 - protected
	)

	if mutated := PruneStaleToolCalls(payload, cfg); !mutated {
		t.Fatalf("expected true, got false")
	}
	messages := payload["messages"].([]interface{})
	if len(messages) != 2 {
		t.Fatalf("expected length 2, got %d", len(messages))
	}

	first := messages[0].(map[string]interface{})
	if first["role"] != "assistant" {
		t.Fatalf("expected role assistant")
	}
	content := first["content"].(string)
	if content != "[System] Tool 'tool_one' was executed previously. Result compacted to save context window." {
		t.Fatalf("unexpected content: %q", content)
	}
	if _, hasName := first["name"]; hasName {
		t.Fatalf("expected no name field on summary message")
	}
}

func TestPruneStaleToolCalls_UnmatchedToolCall(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools: config.PtrTo(true),
		ToolProtectionWindow: 1,
	}

	payload := makePayload(
		map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{
			map[string]interface{}{
				"id": "call_1",
				"function": map[string]interface{}{
					"name": "test_tool",
				},
			},
		}}, // 0 - no matching tool message
		map[string]interface{}{"role": "user", "content": "current message"}, // 1 - protected
	)

	if mutated := PruneStaleToolCalls(payload, cfg); mutated {
		t.Fatalf("expected false for unmatched tool call, got true")
	}
	messages := payload["messages"].([]interface{})
	if len(messages) != 2 {
		t.Fatalf("expected length unchanged, got %d", len(messages))
	}
}

func TestPruneStaleToolCalls_ProtectedPairNotPruned(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools: config.PtrTo(true),
		ToolProtectionWindow: 2,
	}

	payload := makePayload(
		map[string]interface{}{"role": "user", "content": "old"}, // 0 - prunable zone
		map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{
			map[string]interface{}{
				"id": "call_1",
				"function": map[string]interface{}{
					"name": "test_tool",
				},
			},
		}}, // 1 - inside protection window
		map[string]interface{}{"role": "tool", "tool_call_id": "call_1", "content": "result"}, // 2 - inside protection window
	)

	if mutated := PruneStaleToolCalls(payload, cfg); mutated {
		t.Fatalf("expected false for pair inside protection window, got true")
	}
	messages := payload["messages"].([]interface{})
	if len(messages) != 3 {
		t.Fatalf("expected length unchanged, got %d", len(messages))
	}
}

func TestPruneStaleToolCalls_NoToolCallsField(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools: config.PtrTo(true),
		ToolProtectionWindow: 1,
	}

	payload := makePayload(
		map[string]interface{}{"role": "assistant", "content": "regular message"}, // 0
		map[string]interface{}{"role": "user", "content": "current"},              // 1 - protected
	)

	if mutated := PruneStaleToolCalls(payload, cfg); mutated {
		t.Fatalf("expected false for message without tool_calls, got true")
	}
	messages := payload["messages"].([]interface{})
	if len(messages) != 2 {
		t.Fatalf("expected length unchanged, got %d", len(messages))
	}
}

func TestPruneStaleToolCalls_NonMapMessages(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools: config.PtrTo(true),
		ToolProtectionWindow: 1,
	}

	payload := makePayload(
		"not a map", // 0
		42,          // 1
		map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{
			map[string]interface{}{
				"id": "call_1",
				"function": map[string]interface{}{
					"name": "test_tool",
				},
			},
		}}, // 2
		map[string]interface{}{"role": "tool", "tool_call_id": "call_1", "content": "result"}, // 3
		map[string]interface{}{"role": "user", "content": "protected"},                        // 4 - protected
	)

	if mutated := PruneStaleToolCalls(payload, cfg); !mutated {
		t.Fatalf("expected true, got false")
	}
	messages := payload["messages"].([]interface{})
	if len(messages) != 4 {
		t.Fatalf("expected length 4 (non-map entries preserved, pair pruned), got %d", len(messages))
	}

	if messages[0] != "not a map" {
		t.Fatalf("expected first element preserved")
	}
	if messages[1] != 42 {
		t.Fatalf("expected second element preserved")
	}
	summary := messages[2].(map[string]interface{})
	if summary["role"] != "assistant" {
		t.Fatalf("expected third element to be summary assistant message")
	}
	user := messages[3].(map[string]interface{})
	if user["role"] != "user" || user["content"].(string) != "protected" {
		t.Fatalf("expected fourth element to be protected user message")
	}
}

func TestPruneStaleToolCalls_MultiplePrunablePairs(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools: config.PtrTo(true),
		ToolProtectionWindow: 2,
	}

	payload := makePayload(
		map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{
			map[string]interface{}{
				"id": "call_old",
				"function": map[string]interface{}{
					"name": "old_tool",
				},
			},
		}}, // 0 - prunable
		map[string]interface{}{"role": "tool", "tool_call_id": "call_old", "content": "old result"}, // 1
		map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{
			map[string]interface{}{
				"id": "call_mid",
				"function": map[string]interface{}{
					"name": "mid_tool",
				},
			},
		}}, // 2 - prunable
		map[string]interface{}{"role": "tool", "tool_call_id": "call_mid", "content": "mid result"}, // 3
		map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{
			map[string]interface{}{
				"id": "call_new",
				"function": map[string]interface{}{
					"name": "new_tool",
				},
			},
		}}, // 4 - protected
		map[string]interface{}{"role": "tool", "tool_call_id": "call_new", "content": "new result"}, // 5 - protected
	)

	if mutated := PruneStaleToolCalls(payload, cfg); !mutated {
		t.Fatalf("expected true, got false")
	}
	messages := payload["messages"].([]interface{})
	if len(messages) != 4 {
		t.Fatalf("expected length 4, got %d", len(messages))
	}

	m0 := messages[0].(map[string]interface{})
	if m0["content"].(string) != "[System] Tool 'old_tool' was executed previously. Result compacted to save context window." {
		t.Fatalf("unexpected first replacement content: %q", m0["content"])
	}

	m1 := messages[1].(map[string]interface{})
	if m1["content"].(string) != "[System] Tool 'mid_tool' was executed previously. Result compacted to save context window." {
		t.Fatalf("unexpected second replacement content: %q", m1["content"])
	}

	m2 := messages[2].(map[string]interface{})
	if _, ok := m2["tool_calls"]; !ok {
		t.Fatalf("expected protected tool_calls to remain")
	}
	m3 := messages[3].(map[string]interface{})
	if m3["role"] != "tool" {
		t.Fatalf("expected protected tool response to remain")
	}
}

func TestPruneStaleToolCalls_ToolCallWithoutName(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools: config.PtrTo(true),
		ToolProtectionWindow: 1,
	}

	payload := makePayload(
		map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{
			map[string]interface{}{
				"id": "call_1",
			},
		}}, // 0 - no function.name
		map[string]interface{}{"role": "tool", "tool_call_id": "call_1", "content": "result"}, // 1
		map[string]interface{}{"role": "user", "content": "current"},                          // 2 - protected
	)

	if mutated := PruneStaleToolCalls(payload, cfg); !mutated {
		t.Fatalf("expected true, got false")
	}

	messages := payload["messages"].([]interface{})
	first := messages[0].(map[string]interface{})
	content := first["content"].(string)
	if content != "[System] Tool 'call_1' was executed previously. Result compacted to save context window." {
		t.Fatalf("expected fallback to tool_call_id, got: %q", content)
	}
}

func TestPruneStaleToolCalls_EmptyToolCalls(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools: config.PtrTo(true),
		ToolProtectionWindow: 1,
	}

	payload := makePayload(
		map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{}}, // 0 - empty slice
		map[string]interface{}{"role": "user", "content": "current"},               // 1 - protected
	)

	if mutated := PruneStaleToolCalls(payload, cfg); mutated {
		t.Fatalf("expected false for empty tool_calls, got true")
	}
}

func TestPruneStaleToolCalls_NilMessages(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools: config.PtrTo(true),
		ToolProtectionWindow: 4,
	}

	payload := map[string]interface{}{}

	if mutated := PruneStaleToolCalls(payload, cfg); mutated {
		t.Fatalf("expected false for nil messages, got true")
	}
}

func TestPruneStaleToolCalls_PairBrokenByUserMessage(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools: config.PtrTo(true),
		ToolProtectionWindow: 1,
	}

	payload := makePayload(
		map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{
			map[string]interface{}{
				"id": "call_1",
				"function": map[string]interface{}{
					"name": "test_tool",
				},
			},
		}}, // 0 - tool_calls
		map[string]interface{}{"role": "user", "content": "interrupted"},     // 1 - not a tool response
		map[string]interface{}{"role": "user", "content": "current message"}, // 2 - protected
	)

	if mutated := PruneStaleToolCalls(payload, cfg); mutated {
		t.Fatalf("expected false for broken pair (user msg between), got true")
	}
	messages := payload["messages"].([]interface{})
	if len(messages) != 3 {
		t.Fatalf("expected length unchanged, got %d", len(messages))
	}
}

func TestPruneStaleToolCalls_PartialPair(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools: config.PtrTo(true),
		ToolProtectionWindow: 1,
	}

	payload := makePayload(
		map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{
			map[string]interface{}{
				"id": "call_1",
				"function": map[string]interface{}{
					"name": "tool_a",
				},
			},
			map[string]interface{}{
				"id": "call_2",
				"function": map[string]interface{}{
					"name": "tool_b",
				},
			},
		}}, // 0 - two tool calls
		map[string]interface{}{"role": "tool", "tool_call_id": "call_1", "content": "result a"}, // 1
		map[string]interface{}{"role": "user", "content": "missing second tool response"},       // 2
		map[string]interface{}{"role": "user", "content": "current"},                            // 3 - protected
	)

	if mutated := PruneStaleToolCalls(payload, cfg); mutated {
		t.Fatalf("expected false for partial pair, got true")
	}
	messages := payload["messages"].([]interface{})
	if len(messages) != 4 {
		t.Fatalf("expected length unchanged, got %d", len(messages))
	}
}

func TestExtractToolCallID(t *testing.T) {
	tests := []struct {
		name string
		in   interface{}
		want string
	}{
		{"valid ID", map[string]interface{}{"id": "call_123"}, "call_123"},
		{"missing ID", map[string]interface{}{}, ""},
		{"empty ID", map[string]interface{}{"id": ""}, ""},
		{"non-map", "not a map", ""},
		{"nil", nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractToolCallID(tt.in)
			if got != tt.want {
				t.Fatalf("extractToolCallID(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPruneStaleToolCalls_PreserveReasoningContent(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools: config.PtrTo(true),
		ToolProtectionWindow: 4,
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{ // 0: old assistant with tool call + reasoning_content
				"role":    "assistant",
				"content": "Let me check the weather",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":   "call_old",
						"type": "function",
						"function": map[string]interface{}{
							"name":      "get_weather",
							"arguments": "{\"location\":\"Paris\"}",
						},
					},
				},
				"reasoning_content": "I need to call the weather API to get current conditions in Paris",
			},
			map[string]interface{}{ // 1: old tool result
				"role":         "tool",
				"tool_call_id": "call_old",
				"content":      "It's sunny, 22°C",
			},
			map[string]interface{}{ // 2: user follow-up
				"role":    "user",
				"content": "What should I wear?",
			},
			map[string]interface{}{ // 3: assistant reply
				"role":    "assistant",
				"content": "Wear light clothes",
			},
			map[string]interface{}{ // 4: user question
				"role":    "user",
				"content": "And what about dinner?",
			},
			map[string]interface{}{ // 5: assistant reply
				"role":    "assistant",
				"content": "Something warm",
			},
		},
	}

	if mutated := PruneStaleToolCalls(payload, cfg); !mutated {
		t.Fatalf("expected true, got false")
	}

	messages := payload["messages"].([]interface{})
	if len(messages) != 5 {
		t.Fatalf("expected 5 messages (1 replaced + 4 original), got %d", len(messages))
	}

	// First message should be replacement with reasoning_content preserved
	first := messages[0].(map[string]interface{})
	if first["role"] != "assistant" {
		t.Fatalf("expected assistant role, got %v", first["role"])
	}
	content := first["content"].(string)
	if !strings.Contains(content, "Tool 'get_weather' was executed previously") {
		t.Fatalf("expected summary content, got %q", content)
	}
	if rc, ok := first["reasoning_content"].(string); !ok || rc != "I need to call the weather API to get current conditions in Paris" {
		t.Fatalf("expected reasoning_content to be preserved, got %v", rc)
	}

	// Remaining messages should be preserved
	if messages[1].(map[string]interface{})["role"] != "user" {
		t.Fatalf("expected user at index 1")
	}
	if messages[2].(map[string]interface{})["role"] != "assistant" {
		t.Fatalf("expected assistant at index 2")
	}
}
