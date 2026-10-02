package pipeline

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/nenya/config"
)

// toolPair builds an assistant tool_call message paired with its tool result.
func toolPair(name string) []interface{} {
	return []interface{}{
		map[string]interface{}{
			"role":    "assistant",
			"content": "",
			"tool_calls": []interface{}{
				map[string]interface{}{
					"id":       "call_" + name,
					"type":     "function",
					"function": map[string]interface{}{"name": name, "arguments": "{}"},
				},
			},
		},
		map[string]interface{}{"role": "tool", "tool_call_id": "call_" + name, "content": "result-" + name},
	}
}

// thoughtContent builds an assistant message with a prunable reasoning block.
func thoughtContent(reasoning, answer string) string {
	return "<think " + reasoning + " </think " + answer
}

// freshToolConversation builds a conversation with `pairs` tool exchanges,
// optionally preceded by an old assistant think message. Every call returns
// independent maps so replays never alias each other.
func freshToolConversation(pairs int, headThink bool) []interface{} {
	msgs := []interface{}{map[string]interface{}{"role": "system", "content": "sys"}}
	if headThink {
		msgs = append(msgs, map[string]interface{}{
			"role": "assistant", "content": thoughtContent("old reasoning", "older-answer"),
		})
	}
	for i := 0; i < pairs; i++ {
		msgs = append(msgs, toolPair("t"+strconv.Itoa(i))...)
	}
	return msgs
}

func payloadWith(messages []interface{}) map[string]interface{} {
	return map[string]interface{}{"messages": messages}
}

func messagesOf(t *testing.T, payload map[string]interface{}) []interface{} {
	t.Helper()
	msgs, ok := payload["messages"].([]interface{})
	if !ok {
		t.Fatalf("messages missing or wrong type: %T", payload["messages"])
	}
	return msgs
}

// messageContent returns a message's content string with checked assertions.
func messageContent(t *testing.T, msg interface{}) string {
	t.Helper()
	obj, ok := msg.(map[string]interface{})
	if !ok {
		t.Fatalf("message is not a map: %T", msg)
	}
	c, _ := obj["content"].(string)
	return c
}

// deepCopyMessages returns an independent copy of the message list so a test
// can hold a pristine snapshot or feed two replays that never share maps.
func deepCopyMessages(t *testing.T, msgs []interface{}) []interface{} {
	t.Helper()
	b, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	var out []interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}
	return out
}

// TestPruneStaleToolCalls_MutationWindowConfinesMutations pins Phase 013: only
// tool exchanges within the last MutationWindow messages are pruned; older
// exchanges are left byte-identical.
func TestPruneStaleToolCalls_MutationWindowConfinesMutations(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools:      config.PtrTo(true),
		ToolProtectionWindow: 1,
		MutationWindow:       4,
	}

	msgs := freshToolConversation(6, false)
	before := deepCopyMessages(t, msgs)
	// n = 13 (system + 6 pairs). protection=1 -> pruneEnd=12, window=4 ->
	// windowStart=9. Pair t4 (indices 9,10) is inside the window and prunable;
	// pair t3 (indices 7,8) is outside the window and must stay untouched.
	payload := payloadWith(msgs)
	if !PruneStaleToolCalls(payload, cfg) {
		t.Fatal("expected pruning to occur")
	}
	got := messagesOf(t, payload)

	foundSummary := false
	for _, m := range got {
		obj, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		if c, _ := obj["content"].(string); c == "[System] Tool 't4' was executed previously. Result compacted to save context window." {
			foundSummary = true
		}
	}
	if !foundSummary {
		t.Errorf("expected in-window pair t4 to be compacted; got %#v", got)
	}

	// The pre-window pair must be byte-identical to the pristine snapshot.
	if !reflect.DeepEqual(got[7], before[7]) || !reflect.DeepEqual(got[8], before[8]) {
		t.Errorf("pair outside the mutation window was mutated:\ngot    %#v\nbefore %#v", got[7:9], before[7:9])
	}
}

// TestPruneStaleToolCalls_MutationWindowTooSmall verifies that a mutation
// window not larger than the protection window disables pruning entirely.
func TestPruneStaleToolCalls_MutationWindowTooSmall(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools:      config.PtrTo(true),
		ToolProtectionWindow: 1,
		MutationWindow:       1,
	}
	payload := payloadWith(freshToolConversation(3, false))
	if PruneStaleToolCalls(payload, cfg) {
		t.Errorf("expected no pruning when mutation window <= protection window")
	}
}

// TestPruneThoughts_MutationWindowConfinesMutations pins Phase 013 for thought
// pruning: only assistant messages within the last MutationWindow messages are
// rewritten.
func TestPruneThoughts_MutationWindowConfinesMutations(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneThoughts:  config.PtrTo(true),
		MutationWindow: 2,
	}
	oldText := thoughtContent("old reasoning", "visible-old")
	recentText := thoughtContent("new reasoning", "visible-new")
	payload := payloadWith([]interface{}{
		map[string]interface{}{"role": "system", "content": "sys"},
		map[string]interface{}{"role": "assistant", "content": oldText},
		map[string]interface{}{"role": "user", "content": "hi"},
		map[string]interface{}{"role": "assistant", "content": recentText},
	})
	before := deepCopyMessages(t, messagesOf(t, payload))

	if !PruneThoughts(payload, cfg) {
		t.Fatal("expected thought pruning to occur")
	}
	got := messagesOf(t, payload)
	if !reflect.DeepEqual(got[1], before[1]) {
		t.Errorf("assistant message outside the window was mutated: %#v", got[1])
	}
	if c := messageContent(t, got[3]); strings.Contains(c, "<think") || !strings.Contains(c, "visible-new") {
		t.Errorf("in-window assistant message was not pruned: %q", c)
	}
}

// TestHistoryMutation_CacheStableReplay is the Phase 013 cache-stability test:
// the same conversation replayed with one appended message must leave all but
// the tail mutation window byte-identical. Both replays are built from
// independent message maps so a prefix mutation cannot hide behind aliasing.
func TestHistoryMutation_CacheStableReplay(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneStaleTools:      config.PtrTo(true),
		ToolProtectionWindow: 1,
		PruneThoughts:        config.PtrTo(true),
		MutationWindow:       4,
	}

	base := freshToolConversation(5, true) // system + old think + 5 pairs = 12
	baseSnapshot := deepCopyMessages(t, base)

	runA := payloadWith(deepCopyMessages(t, base))
	PruneStaleToolCalls(runA, cfg)
	PruneThoughts(runA, cfg)

	appended := append(deepCopyMessages(t, base), map[string]interface{}{"role": "user", "content": "next"})
	runB := payloadWith(appended)
	PruneStaleToolCalls(runB, cfg)
	PruneThoughts(runB, cfg)

	a := messagesOf(t, runA)
	b := messagesOf(t, runB)

	prefixLen := len(base) - cfg.MutationWindow
	if prefixLen <= 0 {
		t.Fatalf("test conversation too short for the window")
	}
	// The first in-window message is pair t3's assistant; index prefixLen+2 is
	// pair t4's assistant after the sequence shifts. Both must be safe to
	// index, and runB (whose window moved right) must still reach t4.
	if prefixLen+2 >= len(a) || prefixLen+2 >= len(b) {
		t.Fatalf("fixture too short for the boundary assertions: prefixLen=%d len(a)=%d len(b)=%d", prefixLen, len(a), len(b))
	}
	for i := 0; i < prefixLen; i++ {
		if !reflect.DeepEqual(a[i], baseSnapshot[i]) {
			t.Fatalf("prefix message %d was mutated:\ngot      %#v\nsnapshot %#v", i, a[i], baseSnapshot[i])
		}
		if !reflect.DeepEqual(a[i], b[i]) {
			t.Fatalf("prefix message %d changed between replays:\nA=%#v\nB=%#v", i, a[i], b[i])
		}
	}

	// The window boundary is real and positional: index prefixLen is the first
	// in-window assistant (pair t3). runA rewrites it; runB, with one appended
	// message, has moved the window right and leaves it byte-identical.
	if reflect.DeepEqual(a[prefixLen], baseSnapshot[prefixLen]) {
		t.Errorf("runA did not mutate the first in-window message (index %d)", prefixLen)
	}
	if !reflect.DeepEqual(b[prefixLen], baseSnapshot[prefixLen]) {
		t.Errorf("runB mutated a message that left its window (index %d)", prefixLen)
	}
	// runB still mutates a message inside its own (shifted) window.
	if reflect.DeepEqual(b[prefixLen+2], baseSnapshot[prefixLen+2]) {
		t.Errorf("runB did not mutate its in-window message (index %d)", prefixLen+2)
	}
}

// TestThoughtMutation_CacheStableReplay is the Phase 013 cache-stability test
// for thought pruning: older assistant messages stay byte-identical when a
// message is appended, while the decision for a message still inside the
// window is free to change (it is confined to the tail).
func TestThoughtMutation_CacheStableReplay(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneThoughts:  config.PtrTo(true),
		MutationWindow: 2,
	}
	oldText := thoughtContent("old reasoning", "old-answer")
	recentText := thoughtContent("recent reasoning", "recent-answer")
	newBase := func() []interface{} {
		return []interface{}{
			map[string]interface{}{"role": "system", "content": "sys"},
			map[string]interface{}{"role": "assistant", "content": oldText},
			map[string]interface{}{"role": "user", "content": "u1"},
			map[string]interface{}{"role": "assistant", "content": recentText},
			map[string]interface{}{"role": "user", "content": "u2"},
		}
	}
	// n = 5, window = 2 -> start = 3: the recent assistant (index 3) is
	// in-window and pruned; when a message is appended the window moves to
	// start = 4 and index 3 is left byte-identical.

	base := newBase()
	baseSnapshot := deepCopyMessages(t, base)
	runA := payloadWith(newBase())
	if !PruneThoughts(runA, cfg) {
		t.Fatal("expected thought pruning in the first replay")
	}

	appended := append(newBase(), map[string]interface{}{"role": "user", "content": "u3"})
	runB := payloadWith(appended)
	PruneThoughts(runB, cfg)

	a := messagesOf(t, runA)
	b := messagesOf(t, runB)

	prefixLen := len(base) - cfg.MutationWindow
	for i := 0; i < prefixLen; i++ {
		if !reflect.DeepEqual(a[i], baseSnapshot[i]) {
			t.Fatalf("prefix message %d was mutated:\ngot      %#v\nsnapshot %#v", i, a[i], baseSnapshot[i])
		}
		if !reflect.DeepEqual(a[i], b[i]) {
			t.Fatalf("prefix message %d changed between replays:\nA=%#v\nB=%#v", i, a[i], b[i])
		}
	}

	// The old assistant (index 1, outside the window) is never rewritten.
	if c := messageContent(t, a[1]); c != oldText {
		t.Errorf("old assistant mutated: %q", c)
	}
	// The recent assistant is pruned in the first replay but left intact in
	// the second (it moved outside the window): the boundary is real.
	if c := messageContent(t, a[3]); strings.Contains(c, "<think") {
		t.Errorf("in-window assistant was not pruned: %q", c)
	}
	if c := messageContent(t, b[3]); c != recentText {
		t.Errorf("assistant outside the window was mutated: %q", c)
	}
}
