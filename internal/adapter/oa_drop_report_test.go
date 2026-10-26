package adapter

import "testing"

// openAIArrayResponse builds an OpenAI response whose message content is an
// array carrying a text part plus one unsendable non-text part of the given
// type.
func openAIArrayResponse(partType string) map[string]interface{} {
	return map[string]interface{}{
		"id": "cmpl-u", "object": "chat.completion", "model": "m",
		"choices": []interface{}{
			map[string]interface{}{
				"index": 0,
				"message": map[string]interface{}{
					"role": "assistant",
					"content": []interface{}{
						map[string]interface{}{"type": "text", "text": "answer"},
						map[string]interface{}{"type": partType, partType: map[string]interface{}{"x": "y"}},
					},
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]interface{}{"prompt_tokens": 1, "completion_tokens": 1},
	}
}

func containsKind(kinds []string, kind string) bool {
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// TestConvertOpenAIResponseReportsUnsendable pins AUD-33 (NENYA-252): the
// buffered OpenAI-to-Anthropic conversion substitutes "[unsendable content:
// <type>]" markers for parts it cannot represent, but pre-fix those
// fidelity losses never reached the DropReport surfaces — the conversion
// had no report parameter, so the client saw no X-Nenya-Dropped-Content
// header and the unsendable-content metric never fired for this direction.
func TestConvertOpenAIResponseReportsUnsendable(t *testing.T) {
	a := NewAnthropicAdapter()

	report := NewDropReport()
	out := a.ConvertOpenAIResponseToAnthropicBodyReported(openAIArrayResponse("image_url"), report)

	// The visible marker still substitutes in the translated body.
	blocks, ok := out["content"].([]interface{})
	if !ok || len(blocks) != 2 {
		t.Fatalf("content blocks = %v, want text + marker", out["content"])
	}
	marker, _ := blocks[1].(map[string]interface{})
	if marker["text"] != "[unsendable content: image_url]" {
		t.Fatalf("marker = %v, want the unsendable placeholder", marker)
	}

	// AND the kind reaches the report surface.
	if containsKind(report.Kinds(), "image_url") {
		return
	}
	t.Fatalf("report kinds = %v, want image_url recorded (marker without a report is a silent loss)", report.Kinds())
}

// TestConvertOpenAIResponseReportedNilSafe pins that the unreported entry
// point still works (nil report) — existing callers must not regress.
func TestConvertOpenAIResponseReportedNilSafe(t *testing.T) {
	a := NewAnthropicAdapter()
	out := a.ConvertOpenAIResponseToAnthropicBody(openAIArrayResponse("image_url"))
	blocks, ok := out["content"].([]interface{})
	if !ok || len(blocks) != 2 {
		t.Fatalf("content blocks = %v, want text + marker", out["content"])
	}
}
