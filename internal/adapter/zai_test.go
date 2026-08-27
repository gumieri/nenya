package adapter

import (
	"encoding/json"
	"testing"
)

func TestZAIAdapter_NormalizeError(t *testing.T) {
	a := NewZAIAdapter(ZAIAdapterDeps{})

	tests := []struct {
		name string
		code int
		body string
		want ErrorClass
	}{
		{"concurrency_1302", 429, `{"error":{"code":"1302"}}`, ErrorRateLimited},
		{"frequency_1303", 429, `{"error":{"code":"1303"}}`, ErrorRateLimited},
		{"usage_limit_1308", 429, `{"error":{"code":"1308"}}`, ErrorQuotaExhausted},
		{"weekly_limit_1310", 429, `{"error":{"code":"1310"}}`, ErrorQuotaExhausted},
		{"high_traffic_1312", 429, `{"error":{"code":"1312"}}`, ErrorRetryable},
		{"no_subscription_1311", 429, `{"error":{"code":"1311"}}`, ErrorPermanent},
		{"fair_use_1313", 429, `{"error":{"code":"1313"}}`, ErrorPermanent},
		{"unknown_code_429", 429, `{"error":{"code":"9999"}}`, ErrorRateLimited},
		{"generic_429", 429, `{}`, ErrorRateLimited},
		{"generic_500", 500, `{}`, ErrorRetryable},
		{"empty_body_429", 429, ``, ErrorRateLimited},
		{"malformed_json_429", 429, `{invalid`, ErrorRateLimited},
		{"quota_on_403", 403, `{"error":{"code":"1310"}}`, ErrorQuotaExhausted},
		{"concurrency_on_500", 500, `{"error":{"code":"1302"}}`, ErrorRateLimited},
		{"generic_400", 400, `{"error":{"code":"1311"}}`, ErrorPermanent},
		{"context_window_exceeded", 400, `{"error":{"message":"model_context_window_exceeded"}}`, ErrorRetryable},
		{"context_window_exceeded_in_message", 400, `{"error":{"message":"request failed: model_context_window_exceeded for model glm-5"}}`, ErrorRetryable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := a.NormalizeError(tt.code, []byte(tt.body))
			if got != tt.want {
				t.Errorf("NormalizeError(%d, %q) = %v, want %v", tt.code, tt.body, got, tt.want)
			}
		})
	}
}

// testExtractContent returns a message's string content; messages whose
// content is not a string (e.g. content arrays) yield "".
func testExtractContent(msg map[string]interface{}) string {
	c, _ := msg["content"].(string)
	return c
}

func TestZAIAdapter_MutateRequest(t *testing.T) {
	tests := []struct {
		name string
		in   string
		// wantUnchanged asserts the output is byte-identical to the input
		// (sanitize skipped or no-op). Mutually exclusive with wantMessages.
		wantUnchanged bool
		// wantMessages asserts the sanitized output message count and, for
		// each entry, role plus optional exact content ("" to skip check).
		wantMessages [][2]string
	}{
		{
			name:          "already normalized sequence returns body byte-identical",
			in:            `{"model":"glm-5.3","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"},{"role":"assistant","content":"ok"}]}`,
			wantUnchanged: true,
		},
		{
			name:          "tools present skips sanitize entirely",
			in:            `{"model":"glm-5.3","tools":[{"type":"function","function":{"name":"f"}}],"messages":[{"role":"user","content":"a"},{"role":"user","content":"b"}]}`,
			wantUnchanged: true,
		},
		{
			name: "drops orphaned tool message",
			in:   `{"model":"glm-5.3","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"tc1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"tc_orphan","content":"r"},{"role":"user","content":"q"}]}`,
			wantMessages: [][2]string{
				{"assistant", ""},
				{"user", "q"},
			},
		},
		{
			name: "merges consecutive user messages",
			// Leading system message keeps the bridge prepend out of this case.
			in: `{"model":"glm-5.3","messages":[{"role":"system","content":"s"},{"role":"user","content":"a"},{"role":"user","content":"b"}]}`,
			wantMessages: [][2]string{
				{"system", "s"},
				{"user", "a\n\nb"},
			},
		},
		{
			name: "prepends bridge before leading user message",
			in:   `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`,
			wantMessages: [][2]string{
				{"system", "Continue the conversation."},
				{"user", "hi"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewZAIAdapter(ZAIAdapterDeps{ExtractContent: testExtractContent})
			out, err := a.MutateRequest([]byte(tt.in), "glm-5.3", false)
			if err != nil {
				t.Fatalf("MutateRequest() error = %v", err)
			}
			if tt.wantUnchanged {
				if string(out) != tt.in {
					t.Errorf("MutateRequest() modified an unchanged body:\n in: %s\nout: %s", tt.in, out)
				}
				return
			}
			msgs := parseMutatedMessages(t, out)
			if len(msgs) != len(tt.wantMessages) {
				t.Fatalf("MutateRequest() produced %d messages, want %d: %s", len(msgs), len(tt.wantMessages), out)
			}
			for i, want := range tt.wantMessages {
				role, _ := msgs[i]["role"].(string)
				if role != want[0] {
					t.Errorf("message[%d].role = %q, want %q", i, role, want[0])
				}
				if want[1] != "" {
					content, _ := msgs[i]["content"].(string)
					if content != want[1] {
						t.Errorf("message[%d].content = %q, want %q", i, content, want[1])
					}
				}
			}
		})
	}
}

func TestZAIAdapter_MutateRequestGuards(t *testing.T) {
	body := `{"model":"glm-5.3","messages":[{"role":"user","content":"a"},{"role":"user","content":"b"}]}`

	t.Run("nil extractContent returns body unchanged", func(t *testing.T) {
		a := NewZAIAdapter(ZAIAdapterDeps{})
		out, err := a.MutateRequest([]byte(body), "glm-5.3", false)
		if err != nil {
			t.Fatalf("MutateRequest() error = %v", err)
		}
		if string(out) != body {
			t.Errorf("MutateRequest() with nil extractContent modified the body:\n in: %s\nout: %s", body, out)
		}
	})

	t.Run("empty body returns unchanged", func(t *testing.T) {
		a := NewZAIAdapter(ZAIAdapterDeps{ExtractContent: testExtractContent})
		out, err := a.MutateRequest(nil, "glm-5.3", false)
		if err != nil {
			t.Fatalf("MutateRequest() error = %v", err)
		}
		if len(out) != 0 {
			t.Errorf("MutateRequest(nil) = %q, want empty", out)
		}
	})
}

func parseMutatedMessages(t *testing.T, body []byte) []map[string]interface{} {
	t.Helper()
	var payload struct {
		Messages []map[string]interface{} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("failed to unmarshal mutated body %q: %v", body, err)
	}
	return payload.Messages
}
