package infra

import (
	"encoding/json"
	"testing"
)

func TestErrorKind_Retryable(t *testing.T) {
	tests := []struct {
		kind      ErrorKind
		retryable bool
	}{
		{ErrorKindNone, false},
		{ErrorKindContextExceeded, false},
		{ErrorKindRateLimited, true},
		{ErrorKindAuthFailed, false},
		{ErrorKindModelNotFound, false},
		{ErrorKindProviderTimeout, true},
		{ErrorKindProviderError, false},
		{ErrorKindNetworkError, true},
		{ErrorKindPayloadTooLarge, false},
		{ErrorKindInvalidRequest, false},
		{ErrorKindBouncerError, false},
		{ErrorKindInternal, false},
		{ErrorKindQuotaExhausted, true},
		{ErrorKindMCPUnavailable, false},
		{ErrorKindPolicyRefusal, false},
		{ErrorKindUnsupported, false},
	}
	for _, tt := range tests {
		got := tt.kind.Retryable()
		if got != tt.retryable {
			t.Errorf("ErrorKind(%q).Retryable() = %v, want %v", tt.kind, got, tt.retryable)
		}
	}
}

func TestErrorKind_ShouldFailover(t *testing.T) {
	tests := []struct {
		kind           ErrorKind
		shouldFailover bool
	}{
		{ErrorKindNone, false},
		{ErrorKindContextExceeded, false},
		{ErrorKindRateLimited, false},
		{ErrorKindAuthFailed, false},
		{ErrorKindModelNotFound, false},
		{ErrorKindProviderTimeout, true},
		{ErrorKindProviderError, true},
		{ErrorKindNetworkError, true},
		{ErrorKindPayloadTooLarge, false},
		{ErrorKindInvalidRequest, false},
		{ErrorKindBouncerError, false},
		{ErrorKindInternal, false},
		{ErrorKindMCPUnavailable, false},
		{ErrorKindPolicyRefusal, true},
		{ErrorKindUnsupported, false},
	}
	for _, tt := range tests {
		got := tt.kind.ShouldFailover()
		if got != tt.shouldFailover {
			t.Errorf("ErrorKind(%q).ShouldFailover() = %v, want %v", tt.kind, got, tt.shouldFailover)
		}
	}
}

// TestErrorKind_Scope pins NENYA-42's default fault-scope taxonomy: the
// client's payload (request), the credential (account), transport lifecycle
// (connection), upstream side (provider).
func TestErrorKind_Scope(t *testing.T) {
	tests := []struct {
		kind  ErrorKind
		scope ErrorScope
	}{
		{ErrorKindInvalidRequest, ScopeRequest},
		{ErrorKindPayloadTooLarge, ScopeRequest},
		{ErrorKindModelNotFound, ScopeRequest},
		{ErrorKindContextExceeded, ScopeRequest},
		{ErrorKindPolicyRefusal, ScopeRequest},
		{ErrorKindAuthFailed, ScopeAccount},
		{ErrorKindQuotaExhausted, ScopeAccount},
		{ErrorKindRateLimited, ScopeAccount},
		{ErrorKindNetworkError, ScopeConnection},
		{ErrorKindProviderTimeout, ScopeConnection},
		{ErrorKindProviderError, ScopeProvider},
		{ErrorKindBouncerError, ScopeProvider},
		{ErrorKindInternal, ScopeProvider},
		{ErrorKindMCPUnavailable, ScopeProvider},
		{ErrorKindNone, ScopeUnknown},
		{ErrorKindUnsupported, ScopeRequest},
	}
	for _, tt := range tests {
		got := tt.kind.Scope()
		if got != tt.scope {
			t.Errorf("ErrorKind(%q).Scope() = %q, want %q", tt.kind, got, tt.scope)
		}
	}
}

// TestErrorKind_HTTPStatus pins NENYA-199's closed taxonomy: every kind has a
// canonical client-visible status, an unsupported combination is a 422 (a
// well-formed request the gateway cannot serve as asked), and a request-scoped
// condition never falls through to a 500.
func TestErrorKind_HTTPStatus(t *testing.T) {
	tests := []struct {
		kind   ErrorKind
		status int
	}{
		{ErrorKindInvalidRequest, 400},
		{ErrorKindContextExceeded, 400},
		{ErrorKindAuthFailed, 401},
		{ErrorKindPolicyRefusal, 403},
		{ErrorKindInjection, 403},
		{ErrorKindExfil, 403},
		{ErrorKindExfilDetected, 403},
		{ErrorKindBouncerError, 403},
		{ErrorKindModelNotFound, 404},
		{ErrorKindPayloadTooLarge, 413},
		{ErrorKindUnsupported, 422},
		{ErrorKindRateLimited, 429},
		{ErrorKindProviderError, 502},
		{ErrorKindNetworkError, 502},
		{ErrorKindProviderTimeout, 504},
		{ErrorKindQuotaExhausted, 503},
		{ErrorKindMCPUnavailable, 503},
		{ErrorKindInternal, 500},
		{ErrorKindNone, 500},
	}
	for _, tt := range tests {
		if got := tt.kind.HTTPStatus(); got != tt.status {
			t.Errorf("ErrorKind(%q).HTTPStatus() = %d, want %d", tt.kind, got, tt.status)
		}
	}
}

// TestErrorKind_UnsupportedIsRequestScoped pins the unsupported-combination
// semantics: the client's payload is at fault, so the error must not be
// retried, must not fail over, and must not charge provider health.
func TestErrorKind_UnsupportedIsRequestScoped(t *testing.T) {
	if ErrorKindUnsupported.Scope() != ScopeRequest {
		t.Errorf("Scope() = %q, want %q", ErrorKindUnsupported.Scope(), ScopeRequest)
	}
	if ErrorKindUnsupported.Retryable() {
		t.Error("an unsupported combination must not be retryable")
	}
	if ErrorKindUnsupported.ShouldFailover() {
		t.Error("an unsupported combination must not trigger failover")
	}
	if got := ErrorKindUnsupported.HTTPStatus(); got != 422 {
		t.Errorf("HTTPStatus() = %d, want 422", got)
	}
}

func TestErrorResponse_JSONSerialization(t *testing.T) {
	resp := ErrorResponse{
		Error: ErrorBody{
			Message: "test error",
			Type:    "invalid_request_error",
			Code:    "test_code",
		},
		Kind:    ErrorKindRateLimited,
		Request: "req-123",
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}
	var decoded ErrorResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if decoded.Error.Message != "test error" {
		t.Errorf("Message = %q, want %q", decoded.Error.Message, "test error")
	}
	if decoded.Error.Type != "invalid_request_error" {
		t.Errorf("Type = %q", decoded.Error.Type)
	}
	if decoded.Error.Code != "test_code" {
		t.Errorf("Code = %q", decoded.Error.Code)
	}
	if decoded.Kind != ErrorKindRateLimited {
		t.Errorf("Kind = %q", decoded.Kind)
	}
	if decoded.Request != "req-123" {
		t.Errorf("Request = %q", decoded.Request)
	}
}

func TestErrorResponse_JSON_KindOptional(t *testing.T) {
	resp := ErrorResponse{
		Error: ErrorBody{Message: "no kind"},
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}
	var decoded ErrorResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if decoded.Kind != ErrorKindNone {
		t.Errorf("Kind = %q, want empty", decoded.Kind)
	}
}
