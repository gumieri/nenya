package resilience

import (
	"testing"
	"time"
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

// TestClassifyModelNotFoundAndCapacity pins the NENYA-168 taxonomy: a
// model-not-found error is a *target* fault (cool the target down, lock the
// model on it) rather than a client-class error, and capacity/overload wording
// keeps the capacity class.
func TestClassifyModelNotFoundAndCapacity(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		body         string
		wantClass    ErrorClass
		wantLock     bool
		wantCooldown time.Duration
	}{
		{
			name:         "404 is model-not-found",
			status:       404,
			body:         `{"error":{"message":"not found"}}`,
			wantClass:    ErrorClassModelNotFound,
			wantLock:     true,
			wantCooldown: modelNotFoundCooldown,
		},
		{
			name:         "400 naming an unknown model is model-not-found",
			status:       400,
			body:         `{"error":{"message":"unknown model: foo"}}`,
			wantClass:    ErrorClassModelNotFound,
			wantLock:     true,
			wantCooldown: modelNotFoundCooldown,
		},
		{
			name:         "422 naming a deprecated model is model-not-found",
			status:       422,
			body:         `{"error":{"message":"model has been deprecated"}}`,
			wantClass:    ErrorClassModelNotFound,
			wantLock:     true,
			wantCooldown: modelNotFoundCooldown,
		},
		{
			// A 403 is a permission/credential fault: failing over cannot fix
			// it, so it stays auth even when the body mentions a model.
			name:      "403 stays auth even when the body mentions a model",
			status:    403,
			body:      `{"error":{"message":"model is not available for this account"}}`,
			wantClass: ErrorClassAuth,
			wantLock:  true,
		},
		{
			name:      "503 capacity keeps the capacity class",
			status:    503,
			body:      `{"error":{"message":"model is at capacity"}}`,
			wantClass: ErrorClassCapacity,
			wantLock:  true,
		},
		{
			name:      "503 overload keeps the capacity class",
			status:    503,
			body:      `{"type":"overloaded_error","message":"server is overloaded"}`,
			wantClass: ErrorClassCapacity,
			wantLock:  true,
		},
		{
			// Quota wording vetoes model-not-found: a 400 that says the account
			// is out of quota is a billing failure (NENYA-168).
			name:      "400 quota wording vetoes model-not-found",
			status:    400,
			body:      `{"error":{"message":"quota exceeded for unknown model: foo"}}`,
			wantClass: ErrorClassQuota,
			wantLock:  true,
		},
		{
			name:      "400 with no model wording stays a client error",
			status:    400,
			body:      `{"error":{"message":"invalid parameter: temperature"}}`,
			wantClass: ErrorClassUnknown,
			wantLock:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyHTTPError(tt.status, tt.body, 0)
			if got.Class != tt.wantClass {
				t.Fatalf("class = %q, want %q", got.Class, tt.wantClass)
			}
			if got.ShouldLock != tt.wantLock {
				t.Fatalf("ShouldLock = %v, want %v", got.ShouldLock, tt.wantLock)
			}
			if tt.wantCooldown != 0 && got.Cooldown != tt.wantCooldown {
				t.Fatalf("cooldown = %v, want %v", got.Cooldown, tt.wantCooldown)
			}
		})
	}
}
