package proxy

import "testing"

// TestIsNetworkErrorFinishReason pins NENYA-12: the network-failure
// finish_reason variants recognized across upstream providers.
func TestIsNetworkErrorFinishReason(t *testing.T) {
	tests := []struct {
		reason string
		want   bool
	}{
		{"network_error", true},
		{"network-error", true},
		{"network error", true},
		{"NETWORK_ERROR", true},
		{" Network-Error ", true},
		{"stop", false},
		{"length", false},
		{"tool_calls", false},
		{"network", false},
		{"error", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isNetworkErrorFinishReason(tt.reason); got != tt.want {
			t.Errorf("isNetworkErrorFinishReason(%q) = %v; want %v", tt.reason, got, tt.want)
		}
	}
}

func TestTerminalNetworkErrorReason(t *testing.T) {
	tests := []struct {
		name string
		body map[string]interface{}
		want string
	}{
		{
			name: "first choice carries network_error",
			body: map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{"finish_reason": "network_error"},
				},
			},
			want: "network_error",
		},
		{
			name: "second choice carries variant",
			body: map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{"finish_reason": "stop"},
					map[string]interface{}{"finish_reason": "network-error"},
				},
			},
			want: "network-error",
		},
		{
			name: "anthropic-shaped top-level stop_reason",
			body: map[string]interface{}{
				"stop_reason": "network_error",
				"content":     []interface{}{},
			},
			want: "network_error",
		},
		{
			name: "clean stop returns empty",
			body: map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{"finish_reason": "stop"},
				},
			},
			want: "",
		},
		{
			name: "no choices",
			body: map[string]interface{}{"id": "x"},
			want: "",
		},
		{
			name: "malformed choices ignored",
			body: map[string]interface{}{
				"choices": []interface{}{"not-a-map", 42},
			},
			want: "",
		},
		{body: nil, want: "", name: "nil map"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := terminalNetworkErrorReason(tt.body); got != tt.want {
				t.Errorf("terminalNetworkErrorReason() = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestCapturedStreamHasNetworkErrorFinish(t *testing.T) {
	tests := []struct {
		name     string
		captured string
		want     bool
	}{
		{"compact spelling", `data: {"choices":[{"delta":{},"finish_reason":"network_error"}]}`, true},
		{"spaced spelling", `data: {"choices":[{"delta":{},"finish_reason": "network_error"}]}`, true},
		{"hyphen variant", `data: {"choices":[{"delta":{},"finish_reason":"network-error"}]}`, true},
		{"case variant", `data: {"choices":[{"delta":{},"finish_reason":"Network_Error"}]}`, true},
		{"anthropic stop_reason", `{"type":"message_delta","delta":{"stop_reason":"network_error"}}`, true},
		{"clean stop", `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, false},
		{"plain content", `data: {"choices":[{"delta":{"content":"network error occurred"}}]}`, false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := capturedStreamHasNetworkErrorFinish([]byte(tt.captured)); got != tt.want {
				t.Errorf("capturedStreamHasNetworkErrorFinish(%q) = %v; want %v", tt.captured, got, tt.want)
			}
		})
	}
}

// TestIsRetryableClientError_XAICapacity pins the xAI capacity/temporary
// unavailability pattern set: 4xx bodies carrying capacity wording from an
// xAI provider are retryable, while other providers are not.
func TestIsRetryableClientError_XAICapacity(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		status   int
		body     string
		want     bool
	}{
		{"xai capacity 400", "xai", 400, `{"error":{"message":"The service is currently at capacity"}}`, true},
		{"xai temporarily unavailable 400", "xai", 400, `{"error":{"message":"Model temporarily unavailable"}}`, true},
		{"xai overloaded 400", "xai-grok", 400, `{"error":{"message":"service overloaded"}}`, true},
		{"other provider capacity 400", "openai", 400, `{"error":{"message":"The service is currently at capacity"}}`, false},
		{"xai validation 400", "xai", 400, `{"error":{"message":"messages parameter is required"}}`, false},
		{"xai capacity 500", "xai", 500, `{"error":{"message":"at capacity"}}`, false},
		{"xai empty body 400", "xai", 400, ``, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableClientErrorForProvider(tt.status, []byte(tt.body), tt.provider); got != tt.want {
				t.Errorf("isRetryableClientErrorForProvider(%d, %q, %q) = %v; want %v", tt.status, tt.body, tt.provider, got, tt.want)
			}
		})
	}
}

// TestUpstreamBlamePatterns pins NENYA-26: aggregator-relayed upstream
// failures wearing 4xx statuses are retryable for every provider, while
// genuine validation errors are not.
func TestUpstreamBlamePatterns(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"upstream request failed 400", 400, `{"error":{"message":"Upstream request failed"}}`, true},
		{"upstream error 400", 400, `{"error":{"message":"upstream error: connection refused"}}`, true},
		{"upstream unavailable 422", 422, `{"error":"upstream unavailable, try later"}`, true},
		{"upstream is unavailable 400", 400, `{"error":{"message":"upstream is unavailable"}}`, true},
		{"reach upstream 413", 413, `{"error":"could not reach upstream"}`, true},
		{"normal validation 400", 400, `{"error":{"message":"messages is required"}}`, false},
		{"invalid api key 400", 400, `{"error":{"message":"invalid api key"}}`, false},
		{"blame phrase on 500 not matched here", 500, `{"error":"upstream request failed"}`, false},
		{"empty body", 400, ``, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableClientErrorForProvider(tt.status, []byte(tt.body), ""); got != tt.want {
				t.Errorf("isRetryableClientErrorForProvider(%d, %q) = %v; want %v", tt.status, tt.body, got, tt.want)
			}
		})
	}
}

func TestBodyContainsAnyPhrases(t *testing.T) {
	phrases := []string{"Custom_Transient_Failure", "bridge hiccup", "  "}
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"exact phrase", `{"error":{"message":"custom_transient_failure while relaying"}}`, true},
		{"case-insensitive", `{"error":"BRIDGE HICCUP detected"}`, true},
		{"no match", `{"error":"something else"}`, false},
		{"empty body", ``, false},
		{"whitespace-only phrase not counted as match", `{"error":"has a space"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bodyContainsAnyPhrases([]byte(tt.body), phrases); got != tt.want {
				t.Errorf("bodyContainsAnyPhrases(%q) = %v; want %v", tt.body, got, tt.want)
			}
		})
	}
	if bodyContainsAnyPhrases([]byte(`{"error":"x"}`), nil) {
		t.Error("empty phrase list must never match")
	}
	if bodyContainsAnyPhrases([]byte(`{"error":"x"}`), []string{""}) {
		t.Error("empty-string phrase must never match")
	}
}

// TestEmbeddedProviderError pins NENYA-28: HTTP-200 bodies carrying a
// top-level error object (Ollama-style string, OpenAI/Anthropic-style
// object, or exotic scalars) are detected as failed upstream attempts;
// null/empty error fields on legitimate completions are not.
func TestEmbeddedProviderError(t *testing.T) {
	tests := []struct {
		name      string
		body      map[string]interface{}
		wantFound bool
		wantMsg   string
		wantCode  string
	}{
		{"ollama-style string", map[string]interface{}{"error": "model 'x' not found, try pulling it first"}, true, "model 'x' not found, try pulling it first", ""},
		{"openai-style object", map[string]interface{}{"error": map[string]interface{}{"message": "engine stalled", "code": "internal_error"}}, true, "engine stalled", "internal_error"},
		{"object with code only", map[string]interface{}{"error": map[string]interface{}{"code": "overloaded"}}, true, "overloaded", "overloaded"},
		{"null error is not an error", map[string]interface{}{"error": nil, "choices": []interface{}{}}, false, "", ""},
		{"empty string error", map[string]interface{}{"error": ""}, false, "", ""},
		{"empty object error", map[string]interface{}{"error": map[string]interface{}{}}, false, "", ""},
		{"empty array error", map[string]interface{}{"error": []interface{}{}}, false, "", ""},
		{"numeric error surfaced", map[string]interface{}{"error": 500}, true, "500", ""},
		{"no error key", map[string]interface{}{"choices": []interface{}{}}, false, "", ""},
		{name: "nil map", body: nil, wantFound: false, wantMsg: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			details, found := embeddedProviderError(tt.body)
			if found != tt.wantFound {
				t.Fatalf("embeddedProviderError() found = %v; want %v", found, tt.wantFound)
			}
			if found && details.Message != tt.wantMsg {
				t.Errorf("embeddedProviderError() message = %q; want %q", details.Message, tt.wantMsg)
			}
			if found && details.Code != tt.wantCode {
				t.Errorf("embeddedProviderError() code = %q; want %q", details.Code, tt.wantCode)
			}
		})
	}
}

// TestCapturedStreamEndsWithErrorObject pins NENYA-28's SSE rule: only a
// stream whose TERMINAL data line is an error payload (per
// stream.IsStreamErrorPayload: top-level "error" key, "type":"error", flat
// "type":"*_error") counts. Mid-stream error objects superseded by later
// completion events and [DONE]-terminated streams do not match.
func TestCapturedStreamEndsWithErrorObject(t *testing.T) {
	tests := []struct {
		name     string
		captured string
		want     bool
	}{
		{"ends with error object frame", "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"error\":{\"message\":\"engine crashed\"}}\n\n", true},
		{"anthropic type error terminal", "data: {\"choices\":[{\"delta\":{}}]}\n\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n", true},
		{"flat overloaded_error terminal", "data: {\"type\":\"overloaded_error\"}\n\n", true},
		{"mid-stream error then completion", "data: {\"error\":{\"message\":\"transient\"}}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n", false},
		{"error then DONE", "data: {\"error\":{\"message\":\"boom\"}}\n\ndata: [DONE]\n\n", false},
		{"clean completion only", "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n", false},
		{"nested error key in terminal chunk is not top-level", "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}],\"error\":null}\n\n", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := capturedStreamEndsWithErrorObject([]byte(tt.captured)); got != tt.want {
				t.Errorf("capturedStreamEndsWithErrorObject(%q) = %v; want %v", tt.captured, got, tt.want)
			}
		})
	}
}

// TestDefaultRetryableStatusCodes pins HTTP 529 (provider overloaded) as a
// retryable status by default.
func TestDefaultRetryableStatusCodes(t *testing.T) {
	found := false
	for _, code := range defaultRetryableStatusCodes {
		if code == 529 {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 529 (provider overloaded) in defaultRetryableStatusCodes")
	}
}
