package stream

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// NENYA-346: Responses-format SSE events carry model text as a TOP-LEVEL
// STRING delta. The text surfaces must see it; the function-call argument
// delta must stay excluded (the assembler and its guards own that channel —
// a mid-argument redaction would corrupt the reassembled JSON).

func responsesTextDelta(text string) map[string]interface{} {
	return map[string]interface{}{
		"type":          "response.output_text.delta",
		"item_id":       "msg_1",
		"output_index":  float64(0),
		"content_index": float64(0),
		"delta":         text,
	}
}

func responsesArgsDeltaFragment(text string) map[string]interface{} {
	return map[string]interface{}{
		"type":         "response.function_call_arguments.delta",
		"item_id":      "fc_1",
		"output_index": float64(0),
		"delta":        text,
	}
}

// TestResponsesTextDeltaExtractedForGuards pins that the guard text
// extraction reads the Responses text delta.
func TestResponsesTextDeltaExtractedForGuards(t *testing.T) {
	if got := extractExfilContent(responsesTextDelta("hello")); got != "hello" {
		t.Fatalf("extractExfilContent = %q, want %q", got, "hello")
	}
}

// TestResponsesArgsDeltaNotATextSurface pins the channel boundary: the
// function-call argument delta must NOT surface to the text-path guards.
func TestResponsesArgsDeltaNotATextSurface(t *testing.T) {
	if got := extractExfilContent(responsesArgsDeltaFragment(`{"a":1}`)); got != "" {
		t.Fatalf("extractExfilContent read an argument delta as text: %q", got)
	}
	visited := false
	forEachFilterableTextPart(responsesArgsDeltaFragment(`{"a":1}`), func(content string) (string, FilterAction) {
		visited = true
		return content, ActionPass
	})
	if visited {
		t.Fatal("forEachFilterableTextPart visited a function-call argument delta")
	}
}

// TestResponsesTextDeltaVisitedAndRewritten pins that the text walker visits
// the Responses text delta and that rewrites land back in the same field.
func TestResponsesTextDeltaVisitedAndRewritten(t *testing.T) {
	seen := false
	blocked, rewrote := forEachFilterableTextPart(responsesTextDelta("body"), func(content string) (string, FilterAction) {
		seen = true
		return content + "!", ActionRedact
	})
	if !seen || blocked || !rewrote {
		t.Fatalf("visited=%v blocked=%v rewrote=%v, want true/false/true", seen, blocked, rewrote)
	}

	chunk := responsesTextDelta("body")
	setExfilContent(chunk, "redacted")
	if chunk["delta"] != "redacted" {
		t.Fatalf("rewrite landed in %v, want the top-level delta field", chunk["delta"])
	}
}

// TestResponsesArgsDeltaRewriteExcluded pins that the write-back also skips
// argument deltas (it mirrors the extraction boundary).
func TestResponsesArgsDeltaRewriteExcluded(t *testing.T) {
	chunk := responsesArgsDeltaFragment(`{"a":1}`)
	if setExfilContent(chunk, "redacted") {
		t.Fatal("setExfilContent rewrote a function-call argument delta")
	}
	if chunk["delta"] != `{"a":1}` {
		t.Fatalf("argument delta mutated: %v", chunk["delta"])
	}
}

// TestResponsesPassthroughCutInjectsResponsesError pins the mid-cut
// terminal on the nil-transformer passthrough (NENYA-346): a stream that
// delivered Responses text deltas and died without a terminal event gets a
// Responses-NATIVE error event (typed data payload, no [DONE]) — the
// chat-shaped gateway_error payload plus a stray [DONE] data line would
// corrupt a stream the client parses by event name. The text delta must
// also count as content, so the cut classifies as a mid-generation end.
func TestResponsesPassthroughCutInjectsResponsesError(t *testing.T) {
	wire := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"partial text"}`,
		``,
	}, "\n")
	reader := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	var out bytes.Buffer
	if _, err := io.Copy(&out, reader); err != nil {
		t.Fatalf("read: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, `"code":"gateway_error"`) {
		t.Fatalf("no Responses-native injected error on a cut stream:\n%s", body)
	}
	if strings.Contains(body, `"type":"gateway_error"`) {
		t.Fatalf("chat-shaped gateway_error payload on a Responses stream:\n%s", body)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stray [DONE] terminator on a Responses-format stream:\n%s", body)
	}
	if !strings.Contains(body, "partial text") {
		t.Fatalf("the delivered text delta was lost:\n%s", body)
	}
}

// TestResponsesPassthroughCutBeforeAnyTextDelta pins the injection routing
// gate: a stream cut after only response.created (no text delta yet) is
// still Responses-format to its client and gets the Responses-native error
// event, not the chat-shaped gateway_error + [DONE].
func TestResponsesPassthroughCutBeforeAnyTextDelta(t *testing.T) {
	wire := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		``,
	}, "\n")
	reader := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	var out bytes.Buffer
	if _, err := io.Copy(&out, reader); err != nil {
		t.Fatalf("read: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, `"code":"gateway_error"`) {
		t.Fatalf("no Responses-native injected error on a pre-text cut:\n%s", body)
	}
	if strings.Contains(body, `"error":{"message"`) {
		t.Fatalf("chat-shaped gateway_error payload on a Responses stream:\n%s", body)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stray [DONE] terminator on a Responses-format stream:\n%s", body)
	}
}
