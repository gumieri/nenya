package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/nenya/internal/infra"
)

// containsAny checks if the string contains any of the substrings.
func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// classifyError maps upstream responses to error kinds.
func classifyError(statusCode int, body []byte) infra.ErrorKind {
	switch {
	case statusCode == http.StatusTooManyRequests:
		return infra.ErrorKindRateLimited
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return infra.ErrorKindAuthFailed
	case statusCode == http.StatusNotFound:
		return infra.ErrorKindModelNotFound
	case statusCode == http.StatusRequestEntityTooLarge:
		return infra.ErrorKindPayloadTooLarge
	case statusCode == http.StatusBadRequest:
		return inferErrorKind(body)
	case statusCode >= 500:
		return classifyServerError(body)
	case statusCode == 0:
		return infra.ErrorKindNetworkError
	default:
		return infra.ErrorKindInvalidRequest
	}
}

// inferErrorKind tries to determine the error kind from the error body.
func inferErrorKind(body []byte) infra.ErrorKind {
	if len(body) == 0 {
		return infra.ErrorKindInvalidRequest
	}
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return infra.ErrorKindInvalidRequest
	}
	msg := strings.ToLower(parsed.Error.Message)
	switch {
	case containsAny(msg, "context_length", "context length", "max_tokens", "too many tokens"):
		return infra.ErrorKindContextExceeded
	case containsAny(msg, "rate limit", "rate_limit"):
		return infra.ErrorKindRateLimited
	case containsAny(msg, "timeout", "timed out", "deadline exceeded"):
		return infra.ErrorKindProviderTimeout
	default:
		return infra.ErrorKindInvalidRequest
	}
}

// classifyServerError categorizes 5xx upstream responses.
func classifyServerError(body []byte) infra.ErrorKind {
	if len(body) > 0 && bytes.Contains(bytes.ToLower(body), []byte("timeout")) {
		return infra.ErrorKindProviderTimeout
	}
	return infra.ErrorKindProviderError
}

// writeStructuredError writes a structured error response to the HTTP writer.
func writeStructuredError(w http.ResponseWriter, statusCode int, kind infra.ErrorKind, msg string) {
	writeStructuredErrorWithContext(w, statusCode, kind, msg, "", "")
}

// writeStructuredErrorWithContext writes a structured error with optional provider and model context.
func writeStructuredErrorWithContext(w http.ResponseWriter, statusCode int, kind infra.ErrorKind, msg, provider, model string) {
	if w == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	resp := infra.ErrorResponse{
		Error: infra.ErrorBody{
			Message: msg,
		},
		Kind: kind,
	}
	_ = json.NewEncoder(w).Encode(resp)
}
