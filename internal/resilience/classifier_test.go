package resilience

import (
	"testing"

	"github.com/nenya/internal/util"
)

func TestIsContextLengthError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       bool
	}{
		{"anthropic_context_length", 400, `{"error":{"message":"context_length_exceeded: this model's maximum context length is 200000 tokens, but you provided 209302 tokens"}}`, true},
		{"openai_context_length", 400, `{"error":{"message":"This model's maximum context length is 4097 tokens, but you provided 10000 tokens"}}`, true},
		{"generic_payload_size", 413, `{"error":"payload too large"}`, false},
		{"max_context_length", 422, `{"error":{"message":"max_context_length exceeded"}}`, true},
		{"prompt_too_long", 400, `{"error":{"message":"prompt too long"}}`, true},
		{"too_many_tokens", 400, `{"error":{"message":"too many tokens"}}`, true},
		{"rate_limit", 429, `{"error":"rate_limit_exceeded"}`, false},
		{"internal_error", 500, `{"error":"internal server error"}`, false},
		{"invalid_status", 200, `{"success":true}`, false},
		{"empty_body", 400, `{}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := util.IsContextLengthError(tt.statusCode, tt.body)
			if got != tt.want {
				t.Errorf("isContextLengthError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClassifyHTTPErrorWithContextLimit(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		backoffLevel int
		wantClass   ErrorClass
	}{
		{
			name:         "context_length_error_400",
			status:       400,
			body:         `{"error":{"message":"context_length_exceeded"}}`,
			backoffLevel: 0,
			wantClass:    ErrorClassContextLimit,
		},
		{
			name:         "context_length_error_413",
			status:       413,
			body:         `{"error":{"message":"prompt too long"}}`,
			backoffLevel: 0,
			wantClass:    ErrorClassContextLimit,
		},
		{
			name:         "context_length_error_422",
			status:       422,
			body:         `{"error":{"message":"max_context_length exceeded"}}`,
			backoffLevel: 0,
			wantClass:    ErrorClassContextLimit,
		},
		{
			name:         "quota_error_400",
			status:       400,
			body:         `{"error":{"message":"quota exceeded"}}`,
			backoffLevel: 0,
			wantClass:    ErrorClassQuota,
		},
		{
			name:         "auth_error_401",
			status:       401,
			body:         `{"error":{"message":"unauthorized"}}`,
			backoffLevel: 0,
			wantClass:    ErrorClassAuth,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyHTTPError(tt.status, tt.body, tt.backoffLevel)
			if got.Class != tt.wantClass {
				t.Errorf("classifyHTTPError().Class = %v, want %v", got.Class, tt.wantClass)
			}
			if got.Class == ErrorClassContextLimit {
				if got.ShouldLock {
					t.Errorf("context limit errors should not lock, but got ShouldLock=true")
				}
				if got.Cooldown != 0 {
					t.Errorf("context limit errors should have zero cooldown, but got %v", got.Cooldown)
				}
			}
		})
	}
}