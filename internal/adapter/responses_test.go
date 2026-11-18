package adapter

// Pinning tests for the buffered Responses-API → OpenAI chat-completions
// response conversion (NENYA-337, GitHub issue #154): non-streaming clients
// (and the MCP buffered loop's replay) receive the upstream Responses JSON
// converted back into the chat completion shape.

import (
	"encoding/json"
	"strings"
	"testing"
)

func responsesBodyFixture() map[string]interface{} {
	raw := `{
	  "id": "resp_1",
	  "object": "response",
	  "model": "gpt-6-luna",
	  "status": "completed",
	  "output": [
	    {"type": "reasoning", "id": "rs_1", "summary": [{"type": "summary_text", "text": "checking the dir"}]},
	    {"type": "message", "id": "msg_1", "role": "assistant", "status": "completed", "content": [
	      {"type": "output_text", "text": "file1", "annotations": []},
	      {"type": "output_text", "text": " file2", "annotations": []}
	    ]},
	    {"type": "function_call", "id": "fc_1", "call_id": "call_abc", "name": "bash", "arguments": "{\"cmd\":\"ls\"}"}
	  ],
	  "usage": {
	    "input_tokens": 100,
	    "output_tokens": 50,
	    "total_tokens": 150,
	    "input_tokens_details": {"cached_tokens": 64},
	    "output_tokens_details": {"reasoning_tokens": 20}
	  }
	}`
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		panic(err)
	}
	return body
}

// TestConvertOpenAIToResponsesBodyReported_FidelityKinds pins the
// request-conversion fidelity surface directly (NENYA-176): every dropped
// item carries its DropReport kind — assistant reasoning history
// (reasoning_history, dropped by design: the Responses API does not accept
// reasoning echoes), tool results with non-text parts (image_url), explicit
// non-default chat-only params (seed) — and the usage/param coercion clamps
// overflow values to 0 (CWE-190 §7).
func TestConvertOpenAIToResponsesBodyReported_FidelityKinds(t *testing.T) {
	t.Run("assistant reasoning history reported and dropped", func(t *testing.T) {
		report := NewDropReport()
		payload := map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{"role": "user", "content": "go"},
				map[string]interface{}{"role": "assistant", "content": "did it", "reasoning_content": "internal scratch"},
			},
		}
		got := GetResponsesAdapter().ConvertOpenAIToResponsesBodyReported(payload, report)
		assertReported(t, report, "reasoning_history")
		input := got["input"].([]interface{})
		assistant := input[1].(map[string]interface{})
		// The history message keeps its content as an assistant message
		// item; the reasoning text must NOT ride along in any shape.
		if assistant["type"] != "message" || assistant["role"] != "assistant" {
			t.Fatalf("assistant item = %v, want the assistant message item", assistant)
		}
		if blob, _ := json.Marshal(assistant); strings.Contains(string(blob), "internal scratch") {
			t.Errorf("reasoning text leaked into the converted history: %s", blob)
		}
	})
	t.Run("tool result image parts reported", func(t *testing.T) {
		report := NewDropReport()
		payload := map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{"role": "user", "content": "go"},
				map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{
					map[string]interface{}{"id": "c1", "type": "function", "function": map[string]interface{}{"name": "shot", "arguments": "{}"}},
				}},
				map[string]interface{}{"role": "tool", "tool_call_id": "c1", "content": []interface{}{
					map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64,xx"}},
					map[string]interface{}{"type": "text", "text": "screenshot taken"},
				}},
			},
		}
		got := GetResponsesAdapter().ConvertOpenAIToResponsesBodyReported(payload, report)
		assertReported(t, report, "image_url")
		input := got["input"].([]interface{})
		out := input[2].(map[string]interface{})
		if out["output"] != "screenshot taken" {
			t.Errorf("function_call_output output = %v, want the text part only", out["output"])
		}
	})
	t.Run("explicit seed reported; seed absent silent", func(t *testing.T) {
		report := NewDropReport()
		payload := map[string]interface{}{"seed": 0, "messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
		}}
		GetResponsesAdapter().ConvertOpenAIToResponsesBodyReported(payload, report)
		assertReported(t, report, "seed")
	})
	t.Run("usage coercion clamps overflow to zero", func(t *testing.T) {
		if got := responsesToInt(9.3e18); got != 0 {
			t.Errorf("responsesToInt(9.3e18) = %d, want 0 (overflow guard)", got)
		}
		if got := responsesToInt(float64(1 << 63)); got != 0 {
			t.Errorf("responsesToInt(2^63) = %d, want 0 (exact boundary)", got)
		}
		if got := responsesToInt(42.9); got != 42 {
			t.Errorf("responsesToInt(42.9) = %d, want 42", got)
		}
	})
	t.Run("fully-empty assistant message reported", func(t *testing.T) {
		report := NewDropReport()
		payload := map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{"role": "user", "content": "go"},
				map[string]interface{}{"role": "assistant", "content": nil, "tool_calls": []interface{}{}},
			},
		}
		got := GetResponsesAdapter().ConvertOpenAIToResponsesBodyReported(payload, report)
		assertReported(t, report, "message_content")
		input := got["input"].([]interface{})
		if len(input) != 1 {
			t.Errorf("the empty assistant turn must vanish from the input, got %d items", len(input))
		}
	})
}

// assertReported fails when the kind is absent from the report.
func assertReported(t *testing.T, report *DropReport, kind string) {
	t.Helper()
	for _, k := range report.Kinds() {
		if k == kind {
			return
		}
	}
	t.Fatalf("DropReport kinds = %v, want %q recorded", report.Kinds(), kind)
}

// TestResponsesAdapter_ConvertResponsesToOpenAIBody pins the buffered
// conversion: output_text joined into message.content, reasoning summary
// surfaced as reasoning_content (NENYA-176 fidelity — never dropped
// silently), function_call items → tool_calls, finish_reason tool_calls, and
// the usage object remapped into the chat spelling (which the usage tracker,
// calibration loop, and cache-state tracker all key on).
func TestResponsesAdapter_ConvertResponsesToOpenAIBody(t *testing.T) {
	report := NewDropReport()
	got := GetResponsesAdapter().ConvertResponsesToOpenAIBodyReported(responsesBodyFixture(), report)

	if got["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", got["object"])
	}
	if got["id"] != "resp_1" || got["model"] != "gpt-6-luna" {
		t.Errorf("id/model = %v/%v, want resp_1/gpt-6-luna", got["id"], got["model"])
	}

	choices, ok := got["choices"].([]interface{})
	if !ok || len(choices) != 1 {
		t.Fatalf("choices = %v, want one", got["choices"])
	}
	choice := choices[0].(map[string]interface{})
	msg := choice["message"].(map[string]interface{})
	if msg["content"] != "file1 file2" {
		t.Errorf("message.content = %q, want the joined output_text", msg["content"])
	}
	if msg["reasoning_content"] != "checking the dir" {
		t.Errorf("message.reasoning_content = %v, want the reasoning summary text", msg["reasoning_content"])
	}

	tcs, ok := msg["tool_calls"].([]interface{})
	if !ok || len(tcs) != 1 {
		t.Fatalf("tool_calls = %v, want one", msg["tool_calls"])
	}
	tc := tcs[0].(map[string]interface{})
	if tc["id"] != "call_abc" || tc["type"] != "function" {
		t.Errorf("tool_call = %v, want id call_abc type function", tc)
	}
	fn := tc["function"].(map[string]interface{})
	if fn["name"] != "bash" || fn["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("tool_call.function = %v, want bash with the raw arguments", fn)
	}

	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls (a function_call item is present)", choice["finish_reason"])
	}

	usage := got["usage"].(map[string]interface{})
	if !responsesNumEq(usage["prompt_tokens"], 100) || !responsesNumEq(usage["completion_tokens"], 50) || !responsesNumEq(usage["total_tokens"], 150) {
		t.Errorf("usage = %v, want 100/50/150", usage)
	}
	if d := usage["prompt_tokens_details"].(map[string]interface{}); !responsesNumEq(d["cached_tokens"], 64) {
		t.Errorf("prompt_tokens_details = %v, want cached_tokens 64", usage["prompt_tokens_details"])
	}
	if d := usage["completion_tokens_details"].(map[string]interface{}); !responsesNumEq(d["reasoning_tokens"], 20) {
		t.Errorf("completion_tokens_details = %v, want reasoning_tokens 20", usage["completion_tokens_details"])
	}
}

// responsesNumEq compares a decoded/converter number (int or float64) to an
// expected int.
func responsesNumEq(got interface{}, want int) bool {
	switch n := got.(type) {
	case float64:
		return int(n) == want
	case int:
		return n == want
	default:
		return false
	}
}

// TestResponsesAdapter_ConvertResponsesToOpenAI_Incomplete pins the
// truncated-response mapping: status incomplete + max_output_tokens maps to
// finish_reason length.
func TestResponsesAdapter_ConvertResponsesToOpenAI_Incomplete(t *testing.T) {
	raw := `{"id":"resp_2","model":"gpt-6-luna","status":"incomplete",
	  "incomplete_details":{"reason":"max_output_tokens"},
	  "output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}],
	  "usage":{"input_tokens":9,"output_tokens":99,"total_tokens":108}}`
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	got := GetResponsesAdapter().ConvertResponsesToOpenAIBodyReported(body, NewDropReport())
	choice := got["choices"].([]interface{})[0].(map[string]interface{})
	if choice["finish_reason"] != "length" {
		t.Errorf("finish_reason = %v, want length", choice["finish_reason"])
	}
}

// TestResponsesAdapter_ConvertResponsesToOpenAI_Drops pins fidelity
// reporting (NENYA-176): output item types the chat shape cannot represent
// are recorded on the DropReport by kind — never silent.
func TestResponsesAdapter_ConvertResponsesToOpenAI_Drops(t *testing.T) {
	raw := `{"id":"resp_3","model":"gpt-6-luna","status":"completed",
	  "output":[{"type":"web_search_call","id":"ws_1"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],
	  "usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	report := NewDropReport()
	got := GetResponsesAdapter().ConvertResponsesToOpenAIBodyReported(body, report)
	if report.Empty() {
		t.Fatal("unknown output item type was dropped without a DropReport entry")
	}
	found := false
	for _, k := range report.Kinds() {
		if k == "web_search_call" {
			found = true
		}
	}
	if !found {
		t.Errorf("DropReport kinds = %v, want web_search_call recorded", report.Kinds())
	}
	// The representable text still converts.
	choice := got["choices"].([]interface{})[0].(map[string]interface{})
	if msg := choice["message"].(map[string]interface{}); msg["content"] != "hi" {
		t.Errorf("message.content = %v, want hi", msg["content"])
	}
}
