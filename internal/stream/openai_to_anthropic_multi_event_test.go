package stream

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The OpenAI→Anthropic transformer synthesizes MULTIPLE Anthropic events for
// a single upstream chunk (message_start + content_block_start + deltas,
// message_delta + message_stop). The reader must frame each one as its own
// SSE event: pre-fix (AUD-12 / NENYA-220) every event of one chunk was
// concatenated onto ONE `data:` line — `data: {msg_start}{block_start}...` —
// an invalid JSON payload strict clients reject (the whole stream renders
// empty), and `event:` lines were never emitted either.
func TestOpenAIToAnthropicMultiEventFraming(t *testing.T) {
	// One OpenAI content chunk → message_start + content_block_start +
	// content_block_delta. One finish chunk → content_block_stop +
	// message_delta(+usage) + message_stop.
	wire := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"

	reader := NewSSETransformingReader(strings.NewReader(wire), NewOpenAIToAnthropicTransformer(), context.Background())
	out := drainReader(t, reader)

	// Every data line must parse as exactly ONE JSON object (no glued
	// objects on a single data line).
	var events []map[string]any
	for _, line := range strings.Split(out, "\n") {
		if value, isData := SSEDataField([]byte(line)); isData {
			data := strings.TrimSpace(string(value))
			if data == "" || data == "[DONE]" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(data), &m); err != nil {
				t.Fatalf("an emitted data line is not a single JSON object (glued events): %q — %v", line, err)
			}
			events = append(events, m)
		}
	}

	// The full Anthropic event sequence must be present and ordered. The
	// second upstream chunk (content-only delta) converts to a SINGLE
	// content_block_delta object — the json.Valid fast path's frameOneEvent —
	// so the sequence pins the single-object framing too (AUD-12 review: a
	// data-only single object would be skipped by strict Anthropic SDKs).
	wantTypes := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if len(events) != len(wantTypes) {
		t.Fatalf("expected %d Anthropic events, got %d: %+v", len(wantTypes), len(events), events)
	}
	for i, want := range wantTypes {
		got, _ := events[i]["type"].(string)
		if got != want {
			t.Fatalf("event %d = %q, want %q", i, got, want)
		}
	}

	// A strict Anthropic client keys off `event:` lines too — EVERY typed
	// data frame must be preceded by its own event: line (single-object
	// emissions included: message_start and per-delta frames are emitted one
	// object at a time).
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		value, isData := SSEDataField([]byte(line))
		if !isData {
			continue
		}
		data := strings.TrimSpace(string(value))
		if data == "" || data == "[DONE]" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			t.Fatalf("data line is not one JSON object: %q", line)
		}
		typ, _ := frame["type"].(string)
		if typ == "" {
			continue
		}
		if i == 0 || lines[i-1] != "event: "+typ {
			t.Fatalf("typed frame %q is missing its `event: %s` line (prev line %q)", typ, typ, lines[max(0, i-1)])
		}
	}

	// Terminal-state hygiene: the multi-event emission must still register
	// completion (no spurious injected gateway_error at EOF).
	if reader.InjectedError() {
		t.Fatalf("a complete converted stream must not inject a gateway_error; tail: %q", tail(out, 200))
	}
	if !reader.SawFinishReason() {
		t.Fatal("message_delta stop_reason must register as a completion signal")
	}
	if !reader.SawContent() {
		t.Fatal("content_block_delta text must register as content")
	}
}

// TestSSEEventTypeOfCharset pins the event-line hardening: a type value
// carrying characters outside the token charset (a JSON-decoded newline would
// forge extra SSE fields on the client's stream) yields no event: line.
func TestSSEEventTypeOfCharset(t *testing.T) {
	cases := []struct {
		obj  string
		want string
	}{
		{`{"type":"message_start"}`, "message_start"},
		{`{"type":"content_block_delta.v2"}`, "content_block_delta.v2"},
		{`{"type":"x\ny"}`, ""},       // forged newline rejected
		{`{"type":"a b"}`, ""},        // space rejected
		{`{"type":"ev;data: x"}`, ""}, // SSE field forge rejected
		{`{"noType":1}`, ""},          // absent type
	}
	for _, tc := range cases {
		if got := sseEventTypeOf([]byte(tc.obj)); got != tc.want {
			t.Errorf("sseEventTypeOf(%s) = %q, want %q", tc.obj, got, tc.want)
		}
	}
}

// TestSplitConcatenatedJSONRemainder pins the trailing-segment contract: a
// premature `}` (or any non-whitespace tail after the last decodable value)
// is surfaced as remainder — never silently dropped, never re-glued.
func TestSplitConcatenatedJSONRemainder(t *testing.T) {
	objs, remainder, ok := splitConcatenatedJSON([]byte(`{"a":1}}`))
	if !ok || len(objs) != 1 || string(objs[0]) != `{"a":1}` {
		t.Fatalf("objs = %v, ok = %v; want one object", objs, ok)
	}
	if string(remainder) != "}" {
		t.Fatalf("remainder = %q, want }", remainder)
	}

	// Fully undecodable payload: whole-blob fallback (ok=false).
	if _, _, decodable := splitConcatenatedJSON([]byte(`not json at all`)); decodable {
		t.Fatal("an undecodable payload must return ok=false")
	}

	// A single valid object: no remainder.
	objs, remainder, ok = splitConcatenatedJSON([]byte(`{"a":1}`))
	if !ok || len(objs) != 1 || remainder != nil {
		t.Fatalf("single object: objs = %v, remainder = %q, ok = %v", objs, remainder, ok)
	}
}
