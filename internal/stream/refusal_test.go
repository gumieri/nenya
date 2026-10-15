package stream

import "testing"

// TestIsRefusalTerminal pins the NENYA-191 refusal/safeguard terminal shapes:
// OpenAI finish_reason, OpenAI-compat delta stop_reason, Anthropic
// message_delta, Gemini finishReason, and flat shapes. A terminal that names a
// refusal while also carrying renderable output is still true here — the
// OUTPUT test must run first at the decision sites (post-content semantics
// belong to the caller, not the shape classifier).
func TestIsRefusalTerminal(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{
			name:    "openai content_filter finish",
			payload: `{"choices":[{"delta":{},"finish_reason":"content_filter"}]}`,
			want:    true,
		},
		{
			name:    "openai refusal finish",
			payload: `{"choices":[{"delta":{},"finish_reason":"refusal"}]}`,
			want:    true,
		},
		{
			name:    "openai null finish on a handshake chunk",
			payload: `{"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
			want:    false,
		},
		{
			name:    "openai normal stop finish",
			payload: `{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			want:    false,
		},
		{
			name:    "openai tool_calls finish",
			payload: `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			want:    false,
		},
		{
			name:    "openai refusal terminal beside content (caller orders output first)",
			payload: `{"choices":[{"delta":{"content":"partial"},"finish_reason":"content_filter"}]}`,
			want:    true,
		},
		{
			name:    "anthropic message_delta refusal",
			payload: `{"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null},"usage":{"output_tokens":1}}`,
			want:    true,
		},
		{
			name:    "anthropic message_delta end_turn",
			payload: `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":9}}`,
			want:    false,
		},
		{
			name:    "anthropic message_delta max_tokens",
			payload: `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":9}}`,
			want:    false,
		},
		{
			name:    "gemini SAFETY finishReason",
			payload: `{"candidates":[{"finishReason":"SAFETY"}]}`,
			want:    true,
		},
		{
			name:    "gemini prohibited_content finishReason",
			payload: `{"candidates":[{"finishReason":"PROHIBITED_CONTENT"}]}`,
			want:    true,
		},
		{
			// The first-candidate rule mirrors the output test: a later
			// candidate's terminal must not decide the stream.
			name:    "second-candidate SAFETY does not decide",
			payload: `{"candidates":[{"content":{"parts":[{"text":"pending"}]}},{"finishReason":"SAFETY"}]}`,
			want:    false,
		},
		{
			name:    "gemini BLOCKLIST finishReason",
			payload: `{"candidates":[{"finishReason":"BLOCKLIST"}]}`,
			want:    true,
		},
		{
			name:    "gemini STOP finishReason",
			payload: `{"candidates":[{"finishReason":"STOP"}]}`,
			want:    false,
		},
		{
			name:    "flat stop_reason refusal",
			payload: `{"stop_reason":"refusal"}`,
			want:    true,
		},
		{
			name:    "compat delta stop_reason content_filter",
			payload: `{"choices":[{"delta":{"stop_reason":"content_filter"}}]}`,
			want:    true,
		},
		{
			// The first-choice rule: a later choice's terminal must not
			// decide the stream while choices[0] is still pending.
			name:    "second-choice refusal does not decide",
			payload: `{"choices":[{"delta":{"role":"assistant"}},{"delta":{},"finish_reason":"content_filter"}]}`,
			want:    false,
		},
		{
			name:    "upstream error payload is not a refusal terminal",
			payload: `{"error":{"type":"safeguard_error","message":"blocked"}}`,
			want:    false,
		},
		{
			name:    "safeguard stop",
			payload: `{"choices":[{"delta":{},"finish_reason":"safeguard"}]}`,
			want:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed := ParseSSEChunk([]byte(tt.payload))
			if parsed == nil {
				t.Fatalf("payload did not parse: %s", tt.payload)
			}
			if got := IsRefusalTerminal(parsed); got != tt.want {
				t.Fatalf("IsRefusalTerminal(%s) = %v, want %v", tt.payload, got, tt.want)
			}
		})
	}
	if IsRefusalTerminal(nil) {
		t.Error("a nil payload is not a refusal terminal")
	}
}

// TestIsPolicyRefusalPayload pins the policy-worded error rejection shapes.
func TestIsPolicyRefusalPayload(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{
			name:    "safeguard error type",
			payload: `{"error":{"type":"safeguard_error","message":"request blocked"}}`,
			want:    true,
		},
		{
			name:    "content policy message",
			payload: `{"error":{"message":"This request violates our content policy"}}`,
			want:    true,
		},
		{
			name:    "content_filter code",
			payload: `{"error":{"code":"content_filter_block"}}`,
			want:    true,
		},
		{
			name:    "content moderation message",
			payload: `{"error":{"message":"flagged by content moderation"}}`,
			want:    true,
		},
		{
			name:    "bare moderation (a model name) is not policy",
			payload: `{"error":{"message":"model moderation-latest does not exist"}}`,
			want:    false,
		},
		{
			name:    "generic server error is not policy",
			payload: `{"error":{"type":"server_error","message":"upstream failed"}}`,
			want:    false,
		},
		{
			name:    "invalid parameter is not policy",
			payload: `{"error":{"type":"invalid_request_error","message":"invalid parameter: temperature"}}`,
			want:    false,
		},
		{
			// The shape gate: a non-error payload is never a policy rejection.
			name:    "plain message frame is not policy",
			payload: `{"message":"content policy"}`,
			want:    false,
		},
		{
			// "policy" alone must not match; the phrases are multi-word so a
			// model id like text-moderation-latest is not a policy rejection.
			name:    "bare policy word does not match",
			payload: `{"error":{"message":"the policy id is 42"}}`,
			want:    false,
		},
		{
			// Bare "safeguard" is not a marker: a genuine provider fault
			// worded "internal safeguard check failed" keeps its charge.
			name:    "bare safeguard wording is not policy",
			payload: `{"error":{"message":"internal safeguard check failed"}}`,
			want:    false,
		},
		{
			name:    "a model named after a moderation suite is not policy",
			payload: `{"error":{"message":"model text-moderation-latest does not exist"}}`,
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed := ParseSSEChunk([]byte(tt.payload))
			if parsed == nil {
				t.Fatalf("payload did not parse: %s", tt.payload)
			}
			if got := IsPolicyRefusalPayload(parsed); got != tt.want {
				t.Fatalf("IsPolicyRefusalPayload(%s) = %v, want %v", tt.payload, got, tt.want)
			}
		})
	}
	if IsPolicyRefusalPayload(nil) {
		t.Error("a nil payload is not a policy refusal")
	}
}

// TestIsPolicyRefusalText pins the shared wording predicate.
func TestIsPolicyRefusalText(t *testing.T) {
	for _, want := range []string{
		"blocked by our Content Policy",
		"content_policy_violation",
		"the safety filter flagged this",
		"SAFEGUARD_ERROR intervention",
	} {
		if !IsPolicyRefusalText(want) {
			t.Fatalf("IsPolicyRefusalText(%q) = false, want true", want)
		}
	}
	for _, notWant := range []string{"", "unknown model", "internal server error", "the policy id is 42"} {
		if IsPolicyRefusalText(notWant) {
			t.Fatalf("IsPolicyRefusalText(%q) = true, want false", notWant)
		}
	}
}
