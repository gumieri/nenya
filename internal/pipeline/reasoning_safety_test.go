package pipeline

import (
	"strings"
	"testing"

	"github.com/nenya/config"
)

// mixedHistory builds a conversation with reasoning riding on assistant
// turns interleaved with tool calls: the shape reasoning-safe transforms
// must survive atomically.
func mixedHistory() []interface{} {
	return []interface{}{
		map[string]interface{}{"role": "system", "content": "sys"},
		map[string]interface{}{
			"role":              "assistant",
			"content":           "let me check",
			"reasoning_content": "step one: think hard about the problem",
			"tool_calls": []interface{}{
				map[string]interface{}{
					"id":   "call-1",
					"type": "function",
					"function": map[string]interface{}{
						"name":      "lookup",
						"arguments": "{}",
					},
				},
			},
		},
		map[string]interface{}{
			"role":         "tool",
			"tool_call_id": "call-1",
			"content":      "result one",
		},
		map[string]interface{}{
			"role":              "assistant",
			"content":           "final answer after reasoning",
			"reasoning_content": "step two: more reasoning",
		},
		map[string]interface{}{"role": "user", "content": "thanks"},
	}
}

// TestTrimKeepsReasoningAtomicWithItsUnit pins NENYA-150: trimming drops
// whole trim units, so a surviving assistant message keeps its
// reasoning_content and a dropped one takes it along — never one without
// the other.
func TestTrimKeepsReasoningAtomicWithItsUnit(t *testing.T) {
	payload := map[string]interface{}{"messages": mixedHistory()}
	countTokens := func(string) int { return 100 }
	changed, _ := TrimPayload(nil, payload, 250, countTokens, config.ContextConfig{})
	if !changed {
		t.Fatal("fixture must overflow the budget")
	}
	msgs := payload["messages"].([]interface{})
	for _, raw := range msgs {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		rc, hasReasoning := msg["reasoning_content"].(string)
		if !hasReasoning || rc == "" {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			t.Fatalf("reasoning_content on a non-assistant message: %#v", msg)
		}
		// The assistant turn's reasoning survives only alongside the
		// message's own content or tool calls — never orphaned.
		if msg["content"] == nil && msg["tool_calls"] == nil {
			t.Fatalf("orphaned reasoning block: %#v", msg)
		}
	}
	// The trailing user turn (newest, always kept) must still be present.
	last := msgs[len(msgs)-1].(map[string]interface{})
	if last["role"] != "user" {
		t.Fatalf("the newest turn must survive trimming: %#v", last)
	}
	// The newest assistant turn (reasoning-bearing) must survive WITH its
	// reasoning — the loop above would otherwise pass vacuously.
	found := false
	for _, raw := range msgs {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if msg["reasoning_content"] == "step two: more reasoning" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the reasoning-bearing assistant turn must survive the trim: %#v", msgs)
	}
}

// TestStripThoughtBlocksDropsWhole pins block granularity: a closed think
// block is removed whole; an UNTERMINATED one is left intact with whatever
// follows it (NENYA-150: an unverifiable boundary must not swallow the
// answer).
func TestStripThoughtBlocksDropsWhole(t *testing.T) {
	closed := "before <think>secret reasoning</think> after"
	if got := stripThoughtBlocks(closed); got != "before [Reasoning pruned by gateway] after" {
		t.Fatalf("closed block: %q", got)
	}

	unterminated := "before <think>partial reasoning and the answer must survive"
	if got := stripThoughtBlocks(unterminated); got != unterminated {
		t.Fatalf("an unterminated block must be left intact, got %q", got)
	}

	plain := "no blocks here"
	if got := stripThoughtBlocks(plain); got != plain {
		t.Fatalf("plain content must be untouched, got %q", got)
	}

	// A false-prefix boundary ("<thinking!") is not a block opener.
	tricky := "a <thinking! b <think>x</think> c"
	got := stripThoughtBlocks(tricky)
	if strings.Contains(got, "secret") || strings.Contains(got, "<think>x") {
		t.Fatalf("the real block must be dropped whole: %q", got)
	}
	if !strings.HasPrefix(got, "a <thinking! b ") {
		t.Fatalf("the false prefix must survive: %q", got)
	}
}

// TestPruneThoughtsNeverTouchesBlockArrays pins that the tag pruner only
// mutates string content: provider-native block arrays (Anthropic thinking
// blocks riding through) pass byte-identical.
func TestPruneThoughtsNeverTouchesBlockArrays(t *testing.T) {
	enabled := true
	payload := map[string]interface{}{"messages": []interface{}{
		map[string]interface{}{
			"role": "assistant",
			"content": []interface{}{
				map[string]interface{}{"type": "thinking", "thinking": "<think>structured"},
				map[string]interface{}{"type": "text", "text": "visible"},
			},
		},
	}}
	PruneThoughts(payload, config.CompactionConfig{PruneThoughts: &enabled})
	msg := payload["messages"].([]interface{})[0].(map[string]interface{})
	content := msg["content"].([]interface{})
	if len(content) != 2 {
		t.Fatalf("block arrays must pass through untouched: %#v", content)
	}
}
