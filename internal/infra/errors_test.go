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
	}
	for _, tt := range tests {
		got := tt.kind.ShouldFailover()
		if got != tt.shouldFailover {
			t.Errorf("ErrorKind(%q).ShouldFailover() = %v, want %v", tt.kind, got, tt.shouldFailover)
		}
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