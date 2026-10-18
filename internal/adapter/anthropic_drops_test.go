package adapter

import (
	"reflect"
	"strings"
	"testing"
)

// NENYA-176: opaque reasoning and foreign content in Anthropic bodies must
// never be dropped silently through the OpenAI-shape conversion — reasoning
// text surfaces as reasoning_content, and anything the OpenAI shape cannot
// represent is recorded on the DropReport by kind.

func TestConvertAnthropicResponsePreservesReasoning(t *testing.T) {
	a := NewAnthropicAdapter()
	anthropic := map[string]interface{}{
		"model": "claude-test",
		"content": []interface{}{
			map[string]interface{}{
				"type":      "thinking",
				"thinking":  "step one about the puzzle",
				"signature": "foreign-encrypted-blob-942bda9",
			},
			map[string]interface{}{"type": "text", "text": "the answer"},
		},
		"stop_reason": "end_turn",
	}

	report := NewDropReport()
	out := a.ConvertAnthropicToOpenAIBodyReported(anthropic, false, report)

	choice, ok := out["choices"].([]interface{})[0].(map[string]interface{})
	if !ok {
		t.Fatal("missing choice")
	}
	msg, _ := choice["message"].(map[string]interface{})
	if got, _ := msg["reasoning_content"].(string); got != "step one about the puzzle" {
		t.Fatalf("reasoning_content = %q, want the thinking text", got)
	}
	if got, _ := msg["content"].(string); got != "the answer" {
		t.Fatalf("content = %q, want text preserved", got)
	}
	if got := report.Kinds(); !reflect.DeepEqual(got, []string{"thinking_signature"}) {
		t.Fatalf("kinds = %v, want [thinking_signature]", got)
	}
}

func TestConvertAnthropicResponseReportsForeignBlocks(t *testing.T) {
	a := NewAnthropicAdapter()
	anthropic := map[string]interface{}{
		"model": "claude-test",
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "see attachment"},
			map[string]interface{}{
				"type":       "document",
				"source":     map[string]interface{}{"type": "base64", "media_type": "application/pdf", "data": "JVBERi0xLjQ="},
				"ciphertext": "foreign-blob-d2597a2",
			},
			map[string]interface{}{"type": "redacted_thinking", "data": "opaque"},
			map[string]interface{}{"type": "web_search_tool_result", "content": "x"},
		},
		"stop_reason": "end_turn",
	}

	report := NewDropReport()
	_ = a.ConvertAnthropicToOpenAIBodyReported(anthropic, false, report)

	want := []string{"document", "redacted_thinking", "web_search_tool_result"}
	if got := report.Kinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	// No report: identical conversion path stays usable.
	out := a.ConvertAnthropicToOpenAIBody(anthropic, false)
	if out["choices"] == nil {
		t.Fatal("unreported conversion should still produce choices")
	}
}

func TestConvertAnthropicRequestReportsAndPreserves(t *testing.T) {
	a := NewAnthropicAdapter()
	anthropic := map[string]interface{}{
		"model":      "claude-test",
		"max_tokens": 100,
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"content": []interface{}{
					map[string]interface{}{
						"type":      "thinking",
						"thinking":  "earlier reasoning",
						"signature": "encrypted-history-blob",
					},
					map[string]interface{}{"type": "text", "text": "done"},
				},
			},
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "next?"},
					map[string]interface{}{"type": "document", "source": map[string]interface{}{"type": "base64"}},
				},
			},
		},
	}

	report := NewDropReport()
	out := a.ConvertAnthropicRequestToOpenAIBodyReported(anthropic, report)

	msgs, ok := out["messages"].([]interface{})
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %#v, want 2 entries", out["messages"])
	}
	assistant, _ := msgs[0].(map[string]interface{})
	if got, _ := assistant["reasoning_content"].(string); got != "earlier reasoning" {
		t.Fatalf("assistant reasoning_content = %q", got)
	}
	if got := report.Kinds(); !reflect.DeepEqual(got, []string{"document", "thinking_signature"}) {
		t.Fatalf("kinds = %v, want [document thinking_signature]", got)
	}
}

func TestConvertAnthropicRequestMixedToolResultKept(t *testing.T) {
	a := NewAnthropicAdapter()
	anthropic := map[string]interface{}{
		"model":      "claude-test",
		"max_tokens": 10,
		"system": []interface{}{
			map[string]interface{}{"type": "text", "text": "you are a bot"},
			map[string]interface{}{"type": "document", "source": map[string]interface{}{"type": "base64", "data": "JVBERi0="}},
		},
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "turn context"},
					map[string]interface{}{
						"type":         "tool_result",
						"tool_use_id":  "tu_1",
						"tool_content": nil,
						"content": []interface{}{
							map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "base64"}},
							map[string]interface{}{"type": "text", "text": "screenshot text"},
						},
					},
				},
			},
		},
	}

	report := NewDropReport()
	out := a.ConvertAnthropicRequestToOpenAIBodyReported(anthropic, report)

	// The tool_result's text must survive (kept, not dropped), and the
	// unrepresentable system document + image sub-block must be reported.
	msgs, _ := out["messages"].([]interface{})
	if len(msgs) < 2 {
		t.Fatalf("messages = %#v, want system + user", out["messages"])
	}
	system := msgs[0].(map[string]interface{})
	if got, _ := system["content"].(string); got != "you are a bot" {
		t.Fatalf("system content = %q, want the text block flattened", got)
	}
	user := msgs[1].(map[string]interface{})
	var userText string
	switch c := user["content"].(type) {
	case string:
		userText = c
	case []interface{}:
		for _, part := range c {
			if m, ok := part.(map[string]interface{}); ok {
				userText += m["text"].(string) + "\n"
			}
		}
	}
	if !containsStr(userText, "screenshot text") || !containsStr(userText, "turn context") {
		t.Fatalf("user content %#v, want tool_result text kept with turn text", user["content"])
	}
	want := []string{"document", "image"}
	if got := report.Kinds(); !equalStrings(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
}

func containsStr(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

func equalStrings(a, b []string) bool {
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

func TestConvertAnthropicRequestPureToolLoopNoBogusReport(t *testing.T) {
	a := NewAnthropicAdapter()
	anthropic := map[string]interface{}{
		"model":      "claude-test",
		"max_tokens": 10,
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": "tu_1",
						"content": []interface{}{
							map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "base64"}},
							map[string]interface{}{"type": "text", "text": "seen"},
						},
					},
				},
			},
		},
	}

	report := NewDropReport()
	out := a.ConvertAnthropicRequestToOpenAIBodyReported(anthropic, report)

	msgs, _ := out["messages"].([]interface{})
	tool, _ := msgs[0].(map[string]interface{})
	if got, _ := tool["content"].(string); got != "seen" {
		t.Fatalf("tool content = %q, want the kept text", got)
	}
	// The text WAS preserved as the tool message — no tool_result kind may
	// be reported; the dropped image sub-block must be.
	want := []string{"image"}
	if got := report.Kinds(); !equalStrings(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
}

func TestConvertOpenAIResponseReasoningRidesAsText(t *testing.T) {
	a := NewAnthropicAdapter()
	// A synthesized unsigned `thinking` block would be a guaranteed-400 on
	// replay (NENYA-176): the reasoning must ride as visible text.
	openai := map[string]interface{}{
		"id": "resp-1", "object": "chat.completion",
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"finish_reason": "stop",
				"message": map[string]interface{}{
					"role":              "assistant",
					"content":           "the answer",
					"reasoning_content": "hidden chain of thought",
				},
			},
		},
	}
	out := a.ConvertOpenAIResponseToAnthropicBody(openai)
	content, ok := out["content"].([]interface{})
	if !ok || len(content) < 2 {
		t.Fatalf("content = %#v, want reasoning + answer text blocks", out["content"])
	}
	first, _ := content[0].(map[string]interface{})
	if first["type"] == "thinking" {
		t.Fatalf("unsigned thinking block synthesized: %#v", content)
	}
	if text, _ := first["text"].(string); text != "[reasoning] hidden chain of thought" {
		t.Fatalf("first block text = %q, want the reasoning marker", text)
	}
}

// NENYA-186: the gateway injects/clamps max_tokens in memory (int) while a
// decoded client body carries float64 — the conversion must honor both, or
// the model output cap never reaches an Anthropic-format upstream.
func TestConvertOpenAIToAnthropicKeepsIntMaxTokens(t *testing.T) {
	a := NewAnthropicAdapter()
	for _, tc := range []struct {
		name string
		in   interface{}
		want int
	}{
		{name: "gateway-injected int", in: 2048, want: 2048},
		{name: "decoded float64", in: float64(64000), want: 64000},
		{name: "int64", in: int64(1000), want: 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			openai := map[string]interface{}{
				"model":      "gpt-test",
				"max_tokens": tc.in,
				"messages":   []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
			}
			out := a.ConvertOpenAIToAnthropicBody(openai, "claude-test", false, CacheOpts{})
			if got, _ := out["max_tokens"].(int); got != tc.want {
				t.Fatalf("max_tokens = %#v, want %d", out["max_tokens"], tc.want)
			}
		})
	}
}
