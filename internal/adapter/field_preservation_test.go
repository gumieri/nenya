package adapter

import (
	"strings"
	"testing"
)

// TestFieldPreservationAdapterConversions pins the conversion-pair half of
// the NENYA-151 invariant: a client-set sampling knob crosses
// OpenAI→Anthropic verbatim (or maps to the documented target-equivalent),
// and the reasoning_effort→thinking mapping never overwrites a client-set
// thinking config.
func TestFieldPreservationAdapterConversions(t *testing.T) {
	a := NewAnthropicAdapter()
	openai := map[string]interface{}{
		"model":            "claude-sonnet-4",
		"max_tokens":       float64(1024),
		"temperature":      0.42,
		"top_p":            0.9,
		"stop":             []interface{}{"END"},
		"reasoning_effort": "high",
		"thinking":         map[string]interface{}{"type": "enabled", "budget_tokens": 2048},
	}
	converted := a.ConvertOpenAIToAnthropicBody(openai, "claude-sonnet-4", false, CacheOpts{})

	if got := converted["temperature"]; got != 0.42 {
		t.Fatalf("temperature not preserved: %v", got)
	}
	if got := converted["top_p"]; got != 0.9 {
		t.Fatalf("top_p not preserved: %v", got)
	}
	if got := converted["max_tokens"]; got != 1024 {
		t.Fatalf("max_tokens not preserved: %v", got)
	}
	if _, ok := converted["stop_sequences"]; !ok {
		t.Fatal("stop must map to stop_sequences")
	}
	thinking, ok := converted["thinking"].(map[string]interface{})
	if !ok {
		t.Fatalf("the client's thinking config must survive conversion: %#v", converted["thinking"])
	}
	if thinking["budget_tokens"] != 2048 {
		t.Fatalf("the client's thinking config must win over the reasoning_effort mapping: %#v", thinking)
	}
	// Anthropic-shaped only: a zai-shaped map (the shape the sanitize hooks
	// inject into the shared failover payload) must be dropped, not carried
	// into a guaranteed-400 body.
	zai := a.ConvertOpenAIToAnthropicBody(map[string]interface{}{
		"model":    "claude-sonnet-4",
		"thinking": map[string]interface{}{"type": "enabled", "clear_thinking": false},
	}, "claude-sonnet-4", false, CacheOpts{})
	if _, carried := zai["thinking"]; carried {
		t.Fatalf("a non-Anthropic-shaped thinking map must not be carried: %#v", zai["thinking"])
	}

	// Copied, not aliased: mutating the converted body must not touch the
	// caller's payload (reused across failover targets).
	thinking["budget_tokens"] = 999999
	if openai["thinking"].(map[string]interface{})["budget_tokens"] != 2048 {
		t.Fatal("the conversion aliased the caller's thinking map")
	}
}

// TestReasoningBlocksSurviveConversionAtomically pins the NENYA-150
// conversion guarantee on mixed tool+reasoning turns: Anthropic thinking
// blocks are dropped WHOLE (never split, never leaked as text), tool calls
// and text keep their content, and an OpenAI assistant reasoning_content
// field is deliberately dropped (foreign dialect) without disturbing the
// rest of the message.
func TestReasoningBlocksSurviveConversionAtomically(t *testing.T) {
	a := NewAnthropicAdapter()

	// Anthropic-native history → OpenAI: thinking blocks vanish whole.
	anthropic := map[string]interface{}{
		"model":      "claude-sonnet-4",
		"max_tokens": 1024,
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"content": []interface{}{
					map[string]interface{}{"type": "thinking", "thinking": "secret reasoning", "signature": "sig"},
					map[string]interface{}{"type": "redacted_thinking", "data": "redacted"},
					map[string]interface{}{"type": "text", "text": "visible answer"},
					map[string]interface{}{"type": "tool_use", "id": "tu_1", "name": "lookup", "input": map[string]interface{}{"q": "x"}},
				},
			},
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "tool_result", "tool_use_id": "tu_1", "content": "result"},
				},
			},
		},
	}
	openai := a.ConvertAnthropicRequestToOpenAIBody(anthropic)
	msgs := openai["messages"].([]interface{})
	assistant := msgs[0].(map[string]interface{})
	text, _ := assistant["content"].(string)
	if !strings.Contains(text, "visible answer") {
		t.Fatalf("text must survive: %q", text)
	}
	if strings.Contains(text, "secret reasoning") || strings.Contains(text, "redacted") {
		t.Fatalf("thinking content must not leak as text: %q", text)
	}
	if _, has := assistant["tool_calls"]; !has {
		t.Fatalf("tool calls must survive: %#v", assistant)
	}

	// OpenAI history with reasoning_content → Anthropic: the foreign-dialect
	// field is dropped (deliberately, per the invariant table) and the rest
	// of the message converts intact.
	openaiHistory := map[string]interface{}{
		"model": "claude-sonnet-4",
		"messages": []interface{}{
			map[string]interface{}{
				"role":              "assistant",
				"content":           "answer with reasoning",
				"reasoning_content": "deepseek-style reasoning",
			},
		},
	}
	converted := a.ConvertOpenAIToAnthropicBody(openaiHistory, "claude-sonnet-4", false, CacheOpts{})
	cmsgs := converted["messages"].([]interface{})
	cassistant := cmsgs[0].(map[string]interface{})
	if _, has := cassistant["reasoning_content"]; has {
		t.Fatalf("reasoning_content must not cross into the Anthropic body: %#v", cassistant)
	}
	if cassistant["role"] != "assistant" {
		t.Fatalf("message role lost: %#v", cassistant)
	}
}

// TestGeminiMutateRequestOpaqueModelIDs pins the NENYA-146 adapter half:
// an opaque model ID (ARN, slashed, dotted) is never rewritten by the
// case-normalized built-in model map, while a plain case-variant still maps.
func TestGeminiMutateRequestOpaqueModelIDs(t *testing.T) {
	a := NewGeminiAdapter(GeminiAdapterDeps{})

	// Plain case-variant: the convenience mapping applies.
	body := []byte(`{"model":"GEMINI-PRO","messages":[]}`)
	out, err := a.MutateRequest(body, "GEMINI-PRO", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "gemini-2.5-pro") {
		t.Fatalf("plain case-variant must map, got %s", out)
	}

	// Opaque IDs pass through verbatim.
	for _, opaque := range []string{
		"arn:aws:bedrock:us-east-1::foundation-model/deepseek-r1",
		"bedrock/GEMINI-PRO",
		"gemini-3.5-flash.Custom",
	} {
		body := []byte(`{"model":"` + opaque + `","messages":[]}`)
		out, err := a.MutateRequest(body, opaque, false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), `"`+opaque+`"`) {
			t.Fatalf("opaque ID must pass through verbatim: %s", out)
		}
	}
}
