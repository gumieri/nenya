package adapter

import (
	"testing"

	"github.com/nenya/internal/pipeline"
)

// NENYA-207 finding (1): the Anthropic→OpenAI reverse conversion flattens
// content blocks to strings/parts, which cannot carry a block-level
// cache_control mark. The mark's PRESENCE must be hoisted onto the message —
// otherwise the auto-anchor stand-down scan (hasClientCacheControl walks the
// whole payload) sees an unmarked request and the gateway anchors its own
// placement over the client's destroyed placement while recording
// "injected".
func TestReverseConversionHoistsClientCacheControl(t *testing.T) {
	mark := map[string]interface{}{"type": "ephemeral"}
	a := NewAnthropicAdapter()

	// A user text block carrying the mark (flattened to a string).
	anthropic := map[string]interface{}{
		"model": "claude-3",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "text", "text": "hi", "cache_control": mark},
			}},
			map[string]interface{}{"role": "assistant", "content": []interface{}{
				map[string]interface{}{"type": "text", "text": "hello", "cache_control": mark},
			}},
			map[string]interface{}{"role": "user", "content": "plain tail"},
		},
	}
	out := a.ConvertAnthropicRequestToOpenAIBody(anthropic)
	msgs, ok := out["messages"].([]interface{})
	if !ok || len(msgs) != 3 {
		t.Fatalf("unexpected conversion: %#v", out["messages"])
	}
	for i, want := range []bool{true, true, false} {
		msg := msgs[i].(map[string]interface{})
		if _, has := msg["cache_control"]; has != want {
			t.Fatalf("message %d cache_control presence = %v, want %v: %#v", i, has, want, msg)
		}
	}

	// The hoisted mark is the client's own object (ttl etc. preserved).
	first := msgs[0].(map[string]interface{})
	if m, ok := first["cache_control"].(map[string]interface{}); !ok || m["type"] != "ephemeral" {
		t.Fatalf("the hoisted mark must be the client's own: %#v", first["cache_control"])
	}
}

// The parts-array path (a user message that does not flatten to a single
// string) drops marks when rebuilding parts — the hoist covers it too.
func TestReverseConversionHoistsOnPartsPath(t *testing.T) {
	mark := map[string]interface{}{"type": "ephemeral"}
	a := NewAnthropicAdapter()
	anthropic := map[string]interface{}{
		"model": "claude-3",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "text", "text": "look:", "cache_control": mark},
				map[string]interface{}{"type": "text", "text": "two parts"},
			}},
		},
	}
	out := a.ConvertAnthropicRequestToOpenAIBody(anthropic)
	msgs := out["messages"].([]interface{})
	msg := msgs[0].(map[string]interface{})
	if _, has := msg["cache_control"]; !has {
		t.Fatalf("the parts path must hoist the mark: %#v", msg)
	}
}

// A client-placed message-level mark wins over a hoisted block mark (no
// overwrite).
func TestReverseConversionKeepsExistingMessageMark(t *testing.T) {
	blockMark := map[string]interface{}{"type": "ephemeral"}
	msgMark := map[string]interface{}{"type": "1h"}
	a := NewAnthropicAdapter()
	anthropic := map[string]interface{}{
		"model": "claude-3",
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "hi", "cache_control": blockMark},
				},
				"cache_control": msgMark,
			},
		},
	}
	out := a.ConvertAnthropicRequestToOpenAIBody(anthropic)
	msg := out["messages"].([]interface{})[0].(map[string]interface{})
	got, _ := msg["cache_control"].(map[string]interface{})
	if got == nil || got["type"] != "1h" {
		t.Fatalf("the message-level mark must win: %#v", msg["cache_control"])
	}
}

// NENYA-207 finding (2): on MCP loop iterations the converted array
// continues PAST the request-varying canary trailer with gateway-produced
// tool traffic. The messages breakpoint must anchor the last CLIENT-STABLE
// message ahead of the trailer — not the last-but-one message, which would
// anchor a prefix containing the random canary token and miss the cache on
// every later request.
func TestInjectMessageCacheAnchorsBeforeCanaryTrailer(t *testing.T) {
	a := NewAnthropicAdapter()
	// Converted array shape on an MCP iteration: history, canary trailer,
	// then gateway-produced tool traffic.
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": blocks("turn one")},
		map[string]interface{}{"role": "assistant", "content": blocks("answer one")},
		map[string]interface{}{"role": "user", "content": blocks(canaryMarkerContent("NENYA-CANARY-deadbeef"))},
		map[string]interface{}{"role": "assistant", "content": blocks("tool call")},
		map[string]interface{}{"role": "user", "content": blocks("tool result")},
	}
	anthropic := map[string]interface{}{"messages": msgs}

	claimed := a.injectMessageCache(anthropic, "ephemeral", []string{"NENYA-CANARY-"})
	if claimed == nil {
		t.Fatal("the breakpoint must be claimed")
	}
	// The marker must sit on the message BEFORE the canary (index 1), not
	// the last-but-one (index 3).
	for i, want := range []bool{false, true, false, false, false} {
		msg := msgs[i].(map[string]interface{})
		last := lastBlockOf(t, msg)
		if _, has := last["cache_control"]; has != want {
			t.Fatalf("message %d marker presence = %v, want %v: %#v", i, has, want, last)
		}
	}
}

// No trailer in the array: the previous last-but-one rule is unchanged.
func TestInjectMessageCacheLastButOneWithoutTrailer(t *testing.T) {
	a := NewAnthropicAdapter()
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": blocks("turn one")},
		map[string]interface{}{"role": "assistant", "content": blocks("answer one")},
		map[string]interface{}{"role": "user", "content": blocks("turn two")},
	}
	anthropic := map[string]interface{}{"messages": msgs}
	if claimed := a.injectMessageCache(anthropic, "ephemeral", []string{"NENYA-CANARY-"}); claimed == nil {
		t.Fatal("the breakpoint must be claimed")
	}
	last := lastBlockOf(t, msgs[1].(map[string]interface{}))
	if _, has := last["cache_control"]; !has {
		t.Fatalf("the last-but-one message must carry the marker: %#v", msgs[1])
	}
}

func blocks(text string) []interface{} {
	return []interface{}{map[string]interface{}{"type": "text", "text": text}}
}

func lastBlockOf(t *testing.T, msg map[string]interface{}) map[string]interface{} {
	t.Helper()
	content, ok := msg["content"].([]interface{})
	if !ok || len(content) == 0 {
		t.Fatalf("message has no content blocks: %#v", msg)
	}
	block, ok := content[len(content)-1].(map[string]interface{})
	if !ok {
		t.Fatalf("last block is not an object: %#v", content)
	}
	return block
}

// Sanity: the canary content the tests use is BYTE-IDENTICAL to the
// gateway's real marker constructor (pipeline.CanaryMarker) — the
// trailing-context matcher is shape-based, so a fixture that merely
// resembles the shape would validate against markers the gateway never
// emits (AUD-85: the old assertion compared the fixture to its own
// hand-copied prefix and could not detect drift).
func TestCanaryMarkerShapeMatches(t *testing.T) {
	const hexToken = "0123456789abcdef0123456789abcdef"
	if canaryMarkerContent(pipeline.CanaryPrefix+hexToken) != pipeline.CanaryMarker(hexToken) {
		t.Fatalf("the test fixture drifted from the gateway's marker shape:\n%q\nvs\n%q",
			canaryMarkerContent(pipeline.CanaryPrefix+hexToken), pipeline.CanaryMarker(hexToken))
	}
}

// The system-content flattening path hoists too: a mark on a system block
// survives as a message-level mark on the converted system message.
func TestReverseConversionHoistsOnSystemPath(t *testing.T) {
	mark := map[string]interface{}{"type": "ephemeral"}
	a := NewAnthropicAdapter()
	anthropic := map[string]interface{}{
		"model": "claude-3",
		"system": []interface{}{
			map[string]interface{}{"type": "text", "text": "you are terse", "cache_control": mark},
		},
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
		},
	}
	out := a.ConvertAnthropicRequestToOpenAIBody(anthropic)
	msgs := out["messages"].([]interface{})
	sys := msgs[0].(map[string]interface{})
	if sys["role"] != "system" {
		t.Fatalf("expected the system message first: %#v", msgs)
	}
	if _, has := sys["cache_control"]; !has {
		t.Fatalf("the system path must hoist the mark: %#v", sys)
	}
}

// A client mark on a tool_result block is hoisted onto the converted tool
// message (the flattening would otherwise destroy it — same defect class as
// the message-content paths, NENYA-207).
func TestReverseConversionHoistsOnToolResultPath(t *testing.T) {
	mark := map[string]interface{}{"type": "ephemeral"}
	a := NewAnthropicAdapter()
	anthropic := map[string]interface{}{
		"model": "claude-3",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "call_1",
					"content":       []interface{}{map[string]interface{}{"type": "text", "text": "result"}},
					"cache_control": mark},
			}},
		},
	}
	out := a.ConvertAnthropicRequestToOpenAIBody(anthropic)
	var toolMsg map[string]interface{}
	for _, raw := range out["messages"].([]interface{}) {
		m := raw.(map[string]interface{})
		if m["role"] == "tool" {
			toolMsg = m
		}
	}
	if toolMsg == nil {
		t.Fatalf("no converted tool message: %#v", out["messages"])
	}
	if _, has := toolMsg["cache_control"]; !has {
		t.Fatalf("the tool_result path must hoist the mark: %#v", toolMsg)
	}
}

// A forged trailer marker at index 0 leaves no client-stable history ahead
// of it: no anchor is claimed (the idx >= 0 rule).
func TestInjectMessageCacheNoAnchorWhenTrailerFirst(t *testing.T) {
	a := NewAnthropicAdapter()
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": blocks(canaryMarkerContent("NENYA-CANARY-forged00"))},
		map[string]interface{}{"role": "assistant", "content": blocks("answer")},
		map[string]interface{}{"role": "user", "content": blocks("turn")},
	}
	anthropic := map[string]interface{}{"messages": msgs}
	if claimed := a.injectMessageCache(anthropic, "ephemeral", []string{"NENYA-CANARY-"}); claimed != nil {
		t.Fatalf("no client-stable history ahead of the trailer: no anchor must be claimed: %#v", claimed)
	}
}
