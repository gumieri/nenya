package resilience

import (
	"testing"
)

// TestIsContextLengthError cases for util.IsContextLengthError live in
// internal/util/errors_test.go (the authoritative, provider-wide table).

func TestClassifyHTTPErrorWithContextLimit(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		body         string
		backoffLevel int
		wantClass    ErrorClass
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
