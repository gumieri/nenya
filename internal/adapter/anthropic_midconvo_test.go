package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestConvertOpenAI_MidConversationSystemPreserved pins NENYA-30: on models
// that accept mid-conversation system messages (Anthropic 4.8+/5-family),
// mid-array system reminders stay in the messages array with their client
// cache_control breakpoints intact, and only the leading system run is
// hoisted into the top-level system prompt.
func TestConvertOpenAI_MidConversationSystemPreserved(t *testing.T) {
	a := NewAnthropicAdapter()
	openai := map[string]interface{}{
		"model": "claude-sonnet-4-8",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "You are Claude Code."},
			map[string]interface{}{"role": "user", "content": "explore the repo"},
			map[string]interface{}{
				"role": "system",
				"content": []interface{}{
					map[string]interface{}{
						"type":          "text",
						"text":          "system reminder: plan mode active",
						"cache_control": map[string]interface{}{"type": "ephemeral"},
					},
				},
			},
			map[string]interface{}{"role": "assistant", "content": "ok"},
			map[string]interface{}{
				"role": "system",
				"content": []interface{}{
					map[string]interface{}{
						"type":          "text",
						"text":          "system reminder: tests passing",
						"cache_control": map[string]interface{}{"type": "ephemeral", "ttl": "1h"},
					},
				},
			},
			map[string]interface{}{"role": "user", "content": "continue"},
		},
	}

	result := a.ConvertOpenAIToAnthropicBody(openai, "claude-sonnet-4-8", false, CacheOpts{})

	// Only the leading system run is hoisted.
	sys, ok := result["system"].(string)
	if !ok || sys != "You are Claude Code." {
		t.Fatalf("expected top-level system to be the leading prompt only, got %v", result["system"])
	}

	msgs, ok := result["messages"].([]interface{})
	if !ok {
		t.Fatal("expected messages array")
	}
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages (2 system kept in place), got %d: %v", len(msgs), msgs)
	}

	// First mid-array reminder: position + breakpoint preserved.
	m1, ok := msgs[1].(map[string]interface{})
	if !ok || m1["role"] != "system" {
		t.Fatalf("expected system reminder at index 1, got %v", msgs[1])
	}
	blocks1, ok := m1["content"].([]interface{})
	if !ok || len(blocks1) != 1 {
		t.Fatalf("expected verbatim content block, got %v", m1["content"])
	}
	b1, _ := blocks1[0].(map[string]interface{})
	cc1, hasCC := b1["cache_control"].(map[string]interface{})
	if !hasCC || cc1["type"] != "ephemeral" {
		t.Errorf("expected cache_control on first reminder, got %v", b1)
	}

	// Second reminder keeps its own TTL'd breakpoint.
	m2, ok := msgs[3].(map[string]interface{})
	if !ok || m2["role"] != "system" {
		t.Fatalf("expected system reminder at index 3, got %v", msgs[3])
	}
	blocks2, _ := m2["content"].([]interface{})
	b2, _ := blocks2[0].(map[string]interface{})
	cc, ok := b2["cache_control"].(map[string]interface{})
	if !ok || cc["ttl"] != "1h" {
		t.Errorf("expected ttl=1h cache_control on second reminder, got %v", b2)
	}
}

// TestConvertOpenAI_MidConversationSystemLegacyHoist guards the old-model
// fallback: every system message is hoisted into the top-level system
// prompt and the messages array carries none (pre-NENYA-30 behavior).
func TestConvertOpenAI_MidConversationSystemLegacyHoist(t *testing.T) {
	a := NewAnthropicAdapter()
	openai := map[string]interface{}{
		"model": "claude-3-5-sonnet",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "base prompt"},
			map[string]interface{}{"role": "user", "content": "hi"},
			map[string]interface{}{
				"role": "system",
				"content": []interface{}{
					map[string]interface{}{
						"type":          "text",
						"text":          "mid reminder",
						"cache_control": map[string]interface{}{"type": "ephemeral"},
					},
				},
			},
			map[string]interface{}{"role": "user", "content": "bye"},
		},
	}

	result := a.ConvertOpenAIToAnthropicBody(openai, "claude-3-5-sonnet", false, CacheOpts{})

	sys, ok := result["system"].(string)
	if !ok || !strings.Contains(sys, "base prompt") || !strings.Contains(sys, "mid reminder") {
		t.Fatalf("expected legacy hoist of all system text, got %v", result["system"])
	}
	msgs, _ := result["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (no system), got %d: %v", len(msgs), msgs)
	}
}

// TestConvertOpenAI_MidConversationSystemOverride checks that an explicit
// CacheOpts verdict beats the family inference (catalog-driven off-switch).
func TestConvertOpenAI_MidConversationSystemOverride(t *testing.T) {
	a := NewAnthropicAdapter()
	openai := map[string]interface{}{
		"model": "claude-sonnet-4-8",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "base"},
			map[string]interface{}{"role": "user", "content": "hi"},
			map[string]interface{}{"role": "system", "content": "mid"},
			map[string]interface{}{"role": "user", "content": "bye"},
		},
	}

	off := false
	result := a.ConvertOpenAIToAnthropicBody(openai, "claude-sonnet-4-8", false, CacheOpts{MidConversationSystem: &off})
	sys, ok := result["system"].(string)
	if !ok || !strings.Contains(sys, "base") || !strings.Contains(sys, "mid") {
		t.Fatalf("expected explicit override to force legacy hoist, got %v", result["system"])
	}

	on := true
	openai["model"] = "claude-3-5-sonnet"
	result = a.ConvertOpenAIToAnthropicBody(openai, "claude-3-5-sonnet", false, CacheOpts{MidConversationSystem: &on})
	msgs, _ := result["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Fatalf("expected explicit override to keep mid-array system, got %d messages", len(msgs))
	}
}

// TestConvertOpenAI_SystemBreakpointOnHoistedRun checks the legacy hoist
// still lands the configured breakpoint on the top-level system block
// (issue step: cache breakpoint placement on hoisted system preserved).
func TestConvertOpenAI_SystemBreakpointOnHoistedRun(t *testing.T) {
	a := NewAnthropicAdapter()
	openai := map[string]interface{}{
		"model": "claude-3-5-sonnet",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "static prompt"},
			map[string]interface{}{"role": "user", "content": "hi"},
			map[string]interface{}{"role": "system", "content": "reminder"},
			map[string]interface{}{"role": "user", "content": "bye"},
		},
	}
	result := a.ConvertOpenAIToAnthropicBody(openai, "claude-3-5-sonnet", false, CacheOpts{System: true})
	blocks, ok := result["system"].([]interface{})
	if !ok || len(blocks) != 1 {
		t.Fatalf("expected single hoisted system block, got %v", result["system"])
	}
	if _, ok := blocks[0].(map[string]interface{})["cache_control"]; !ok {
		t.Error("expected cache_control on hoisted system block")
	}
}

// TestReverseConvert_PreservesMidConversationSystem pins the ingress side:
// Anthropic-format clients (Claude Code) send mid-array system reminders
// with cache_control; the OpenAI-format conversion must keep the blocks
// verbatim or the egress re-conversion loses the breakpoint.
func TestReverseConvert_PreservesMidConversationSystem(t *testing.T) {
	a := NewAnthropicAdapter()
	anthropic := map[string]interface{}{
		"model":      "claude-sonnet-4-8",
		"max_tokens": 1024,
		"system":     "top-level prompt",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
			map[string]interface{}{
				"role": "system",
				"content": []interface{}{
					map[string]interface{}{
						"type":          "text",
						"text":          "reminder",
						"cache_control": map[string]interface{}{"type": "ephemeral"},
					},
				},
			},
			map[string]interface{}{"role": "assistant", "content": "hello"},
		},
	}

	openai := a.ConvertAnthropicRequestToOpenAIBody(anthropic)
	raw, err := json.Marshal(openai)
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, m := range decoded.Messages {
		if m["role"] != "system" {
			continue
		}
		found = true
		blocks, ok := m["content"].([]any)
		if !ok {
			// The hoisted top-level prompt arrives as a string; only
			// mid-array reminders carry verbatim blocks.
			continue
		}
		if len(blocks) != 1 {
			t.Fatalf("expected verbatim system block, got %v", m["content"])
		}
		block, _ := blocks[0].(map[string]any)
		if block["text"] != "reminder" {
			t.Errorf("unexpected reminder text: %v", block["text"])
		}
		cc, ok := block["cache_control"].(map[string]any)
		if !ok || cc["type"] != "ephemeral" {
			t.Errorf("expected cache_control to survive ingress, got %v", block["cache_control"])
		}
	}
	if !found {
		t.Fatal("expected mid-array system message to survive ingress")
	}
}

// TestConvert_RoundTripClaudeCodeShape runs the full ingress→egress chain
// for a Claude Code-shaped payload and asserts the moving breakpoint is
// still on the wire toward the upstream Anthropic API.
func TestConvert_RoundTripClaudeCodeShape(t *testing.T) {
	a := NewAnthropicAdapter()
	ingress := map[string]interface{}{
		"model":      "claude-sonnet-4-8",
		"max_tokens": 1024,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "fix the bug"},
			map[string]interface{}{
				"role": "system",
				"content": []interface{}{
					map[string]interface{}{
						"type":          "text",
						"text":          "system reminder: plan mode",
						"cache_control": map[string]interface{}{"type": "ephemeral"},
					},
				},
			},
		},
	}

	openai := a.ConvertAnthropicRequestToOpenAIBody(ingress)
	egress := a.ConvertOpenAIToAnthropicBody(openai, "claude-sonnet-4-8", false, CacheOpts{})

	msgs, ok := egress["messages"].([]interface{})
	if !ok || len(msgs) != 2 {
		t.Fatalf("expected user + system on egress, got %v", egress["messages"])
	}
	sysMsg, ok := msgs[1].(map[string]interface{})
	if !ok || sysMsg["role"] != "system" {
		t.Fatalf("expected system reminder second on egress, got %v", msgs[1])
	}
	blocks, _ := sysMsg["content"].([]interface{})
	block, _ := blocks[0].(map[string]interface{})
	if _, ok := block["cache_control"]; !ok {
		t.Error("expected cache_control to survive the full round trip")
	}
	if block["text"] != "system reminder: plan mode" {
		t.Errorf("unexpected reminder text: %v", block["text"])
	}
}
