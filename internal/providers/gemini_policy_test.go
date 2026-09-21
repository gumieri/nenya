package providers

import "testing"

func unsignedPayload(model string) map[string]interface{} {
	return map[string]interface{}{
		"model": model,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
			map[string]interface{}{
				"role": "assistant",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":       "call_1",
						"type":     "function",
						"function": map[string]interface{}{"name": "get_answer", "arguments": "{}"},
					},
				},
			},
			map[string]interface{}{"role": "tool", "tool_call_id": "call_1", "content": "42"},
			map[string]interface{}{"role": "user", "content": "thanks"},
		},
	}
}

func payloadWithFlatSignature(model, key string) map[string]interface{} {
	p := unsignedPayload(model)
	asst := p["messages"].([]interface{})[1].(map[string]interface{})
	tc := asst["tool_calls"].([]interface{})[0].(map[string]interface{})
	tc[key] = "real-signature"
	return p
}

func countRole(t *testing.T, payload map[string]interface{}, role string) int {
	t.Helper()
	msgs, ok := payload["messages"].([]interface{})
	if !ok {
		t.Fatal("messages not an array")
	}
	n := 0
	for _, m := range msgs {
		msg, _ := m.(map[string]interface{})
		if msg["role"] == role {
			n++
		}
	}
	return n
}

// TestThoughtSigPolicy_StripDefaultOnGemini3 pins the default policy: an
// unsigned tool call and its paired response are stripped on Gemini 3+.
func TestThoughtSigPolicy_StripDefaultOnGemini3(t *testing.T) {
	deps := geminiDeps()
	payload := unsignedPayload("gemini-3-pro")
	geminiSanitize(deps, payload)

	if countRole(t, payload, "tool") != 0 {
		t.Fatalf("expected unsigned pair stripped by default, got %v", payload["messages"])
	}
}

// TestThoughtSigPolicy_VersionGate pins the version gate: a Gemini 2.5
// model with an unsigned call keeps its history — signatures are advisory
// pre-3 and stripping was gratuitous loss.
func TestThoughtSigPolicy_VersionGate(t *testing.T) {
	deps := geminiDeps()
	payload := unsignedPayload("gemini-2.5-pro")
	geminiSanitize(deps, payload)

	if countRole(t, payload, "tool") != 1 {
		t.Fatalf("expected 2.5 history untouched, got %v", payload["messages"])
	}
}

// TestThoughtSigPolicy_FlatSpellingsSurvive pins the spelling parity fix:
// a client echoing Google's flat thought_signature / thoughtSignature keys
// carries a real signature — the pair must not be stripped, and the flat
// key is normalized into canonical extra_content form.
func TestThoughtSigPolicy_FlatSpellingsSurvive(t *testing.T) {
	for _, key := range []string{"thought_signature", "thoughtSignature"} {
		deps := geminiDeps()
		payload := payloadWithFlatSignature("gemini-3-pro", key)
		geminiSanitize(deps, payload)

		if got := countRole(t, payload, "tool"); got != 1 {
			t.Errorf("%s: flat-signed pair must survive, tool messages = %d", key, got)
			continue
		}
		asst := payload["messages"].([]interface{})[1].(map[string]interface{})
		tc := asst["tool_calls"].([]interface{})[0].(map[string]interface{})
		if _, still := tc[key]; still {
			t.Errorf("%s: flat key must be normalized away", key)
		}
		extra, ok := tc["extra_content"].(map[string]interface{})
		if !ok {
			t.Errorf("%s: expected canonical extra_content, got %v", key, tc["extra_content"])
			continue
		}
		google, _ := extra["google"].(map[string]interface{})
		if google["thought_signature"] != "real-signature" {
			t.Errorf("%s: signature not preserved in extra_content.google: %v", key, extra)
		}
	}
}

// TestThoughtSigPolicy_PlaceholderMode pins the fail-open mode: unsigned
// calls on Gemini 3+ keep their turn and gain the upstream-tolerated skip
// placeholder instead of being excised.
func TestThoughtSigPolicy_PlaceholderMode(t *testing.T) {
	deps := geminiDeps()
	deps.ThoughtSignaturePolicy = "placeholder"
	payload := unsignedPayload("gemini-3-pro")
	geminiSanitize(deps, payload)

	if countRole(t, payload, "tool") != 1 {
		t.Fatalf("expected pair kept under placeholder policy, got %v", payload["messages"])
	}
	asst := payload["messages"].([]interface{})[1].(map[string]interface{})
	tc := asst["tool_calls"].([]interface{})[0].(map[string]interface{})
	extra, ok := tc["extra_content"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected injected placeholder extra_content, got %v", tc["extra_content"])
	}
	google, _ := extra["google"].(map[string]interface{})
	if google["thought_signature"] != "skip_thought_signature_validator" {
		t.Errorf("expected skip placeholder, got %v", google)
	}
}

// TestThoughtSigPolicy_PassthroughMode pins the do-nothing mode: unsigned
// history is forwarded verbatim (documented failure mode: Gemini 3 will
// 400, which surfaces through the normal retry classification).
func TestThoughtSigPolicy_PassthroughMode(t *testing.T) {
	deps := geminiDeps()
	deps.ThoughtSignaturePolicy = "passthrough"
	payload := unsignedPayload("gemini-3-pro")
	geminiSanitize(deps, payload)

	if countRole(t, payload, "tool") != 1 {
		t.Fatalf("expected unsigned pair forwarded under passthrough, got %v", payload["messages"])
	}
	asst := payload["messages"].([]interface{})[1].(map[string]interface{})
	tc := asst["tool_calls"].([]interface{})[0].(map[string]interface{})
	if _, has := tc["extra_content"]; has {
		t.Error("passthrough must not inject anything")
	}
}

// TestThoughtSigPolicy_PlaceholderOnlyOnGemini3 checks the placeholder is
// not injected on pre-3 models (nothing is needed there).
func TestThoughtSigPolicy_PlaceholderOnlyOnGemini3(t *testing.T) {
	deps := geminiDeps()
	deps.ThoughtSignaturePolicy = "placeholder"
	payload := unsignedPayload("gemini-2.5-pro")
	geminiSanitize(deps, payload)

	if countRole(t, payload, "tool") != 1 {
		t.Fatalf("expected 2.5 history untouched, got %v", payload["messages"])
	}
	asst := payload["messages"].([]interface{})[1].(map[string]interface{})
	tc := asst["tool_calls"].([]interface{})[0].(map[string]interface{})
	if _, has := tc["extra_content"]; has {
		t.Error("no placeholder needed on pre-3 models")
	}
}
