package providers

import (
	"testing"
)

func roles(messages []interface{}) []string {
	out := make([]string, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			out = append(out, "?")
			continue
		}
		role, _ := msg["role"].(string)
		out = append(out, role)
	}
	return out
}

func rolesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestGeminiDemote_MidSessionSystem pins the NENYA-44 core rule: only the
// leading system run anchors the token-0 prefix; a mid-session system
// message is demoted to a user turn with the [system] role marker.
func TestGeminiDemote_MidSessionSystem(t *testing.T) {
	deps := geminiDeps()
	messages := []interface{}{
		map[string]interface{}{"role": "system", "content": "lead prompt"},
		map[string]interface{}{"role": "user", "content": "hi"},
		map[string]interface{}{"role": "system", "content": "mid reminder"},
		map[string]interface{}{"role": "assistant", "content": "hello"},
		map[string]interface{}{"role": "user", "content": "bye"},
	}
	payload := map[string]interface{}{"messages": messages}

	geminiSanitize(deps, payload)
	got := payload["messages"].([]interface{})

	if !rolesEqual(roles(got), []string{"system", "user", "user", "assistant", "user"}) {
		t.Fatalf("unexpected roles after demotion: %v", roles(got))
	}
	if got[0].(map[string]interface{})["content"] != "lead prompt" {
		t.Error("leading system message must stay untouched")
	}
	demoted := got[2].(map[string]interface{})
	if demoted["content"] != "[system] mid reminder" {
		t.Errorf("expected marker-prefixed demoted content, got %v", demoted["content"])
	}
}

// TestGeminiDemote_ToolRunBuffering checks that a notice arriving between
// an assistant tool_calls message and its tool results is buffered and
// flushed before the next user turn, keeping the tool run contiguous.
func TestGeminiDemote_ToolRunBuffering(t *testing.T) {
	deps := geminiDeps()
	messages := []interface{}{
		map[string]interface{}{"role": "system", "content": "lead"},
		map[string]interface{}{"role": "user", "content": "run the tool"},
		map[string]interface{}{
			"role": "assistant",
			"tool_calls": []interface{}{
				map[string]interface{}{
					"id":   "call_1",
					"type": "function",
					"function": map[string]interface{}{
						"name":      "get_answer",
						"arguments": "{}",
					},
					"extra_content": map[string]interface{}{"thought_signature": "sig"},
				},
			},
		},
		map[string]interface{}{"role": "tool", "tool_call_id": "call_1", "content": "42"},
		map[string]interface{}{"role": "system", "content": "notice during run"},
		map[string]interface{}{"role": "user", "content": "thanks"},
	}
	payload := map[string]interface{}{"messages": messages}

	geminiSanitize(deps, payload)
	got := payload["messages"].([]interface{})

	if !rolesEqual(roles(got), []string{"system", "user", "assistant", "tool", "user", "user"}) {
		t.Fatalf("unexpected roles: %v", roles(got))
	}
	demoted := got[4].(map[string]interface{})
	if demoted["content"] != "[system] notice during run" {
		t.Errorf("expected buffered notice flushed before user turn, got %v", demoted)
	}
}

// TestGeminiDemote_ArrayContentPreserved checks block-array notices gain a
// marker block while keeping their original blocks verbatim.
func TestGeminiDemote_ArrayContentPreserved(t *testing.T) {
	deps := geminiDeps()
	original := []interface{}{
		map[string]interface{}{"type": "text", "text": "reminder body"},
	}
	messages := []interface{}{
		map[string]interface{}{"role": "system", "content": "lead"},
		map[string]interface{}{"role": "user", "content": "hi"},
		map[string]interface{}{"role": "system", "content": original},
	}
	payload := map[string]interface{}{"messages": messages}

	geminiSanitize(deps, payload)
	got := payload["messages"].([]interface{})

	demoted := got[2].(map[string]interface{})
	blocks, ok := demoted["content"].([]interface{})
	if !ok || len(blocks) != 2 {
		t.Fatalf("expected marker block + original block, got %v", demoted["content"])
	}
	marker, _ := blocks[0].(map[string]interface{})
	if marker["text"] != "[system]" {
		t.Errorf("expected [system] marker block, got %v", marker)
	}
	kept, _ := blocks[1].(map[string]interface{})
	if kept["text"] != "reminder body" {
		t.Error("original block must be preserved verbatim")
	}
}

// TestGeminiDemote_NoticeAtEndAfterToolRun flushes trailing buffered
// notices at the end of the conversation.
func TestGeminiDemote_NoticeAtEndAfterToolRun(t *testing.T) {
	deps := geminiDeps()
	messages := []interface{}{
		map[string]interface{}{"role": "system", "content": "lead"},
		map[string]interface{}{"role": "user", "content": "go"},
		map[string]interface{}{
			"role": "assistant",
			"tool_calls": []interface{}{
				map[string]interface{}{
					"id":   "call_1",
					"type": "function",
					"function": map[string]interface{}{
						"name":      "get_answer",
						"arguments": "{}",
					},
					"extra_content": map[string]interface{}{"thought_signature": "sig"},
				},
			},
		},
		map[string]interface{}{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
		map[string]interface{}{"role": "system", "content": "final notice"},
	}
	payload := map[string]interface{}{"messages": messages}

	geminiSanitize(deps, payload)
	got := payload["messages"].([]interface{})

	if !rolesEqual(roles(got), []string{"system", "user", "assistant", "tool", "user"}) {
		t.Fatalf("unexpected roles: %v", roles(got))
	}
	if last := got[4].(map[string]interface{}); last["content"] != "[system] final notice" {
		t.Errorf("expected trailing notice flushed at end, got %v", last)
	}
}

// TestGeminiDemote_LeadingRunUntouched checks all-system-head payloads and
// conversations without mid-session system messages are left alone.
func TestGeminiDemote_LeadingRunUntouched(t *testing.T) {
	deps := geminiDeps()

	allHead := []interface{}{
		map[string]interface{}{"role": "system", "content": "a"},
		map[string]interface{}{"role": "system", "content": "b"},
		map[string]interface{}{"role": "user", "content": "hi"},
	}
	if demoteGeminiMidSessionSystem(deps, allHead) {
		t.Error("all-leading system run must not trigger demotion")
	}

	noSystem := []interface{}{
		map[string]interface{}{"role": "user", "content": "hi"},
		map[string]interface{}{"role": "assistant", "content": "hello"},
	}
	if demoteGeminiMidSessionSystem(deps, noSystem) {
		t.Error("conversation without system messages must not trigger demotion")
	}
}
