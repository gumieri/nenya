package auth

import (
	"fmt"
	"net/http"
	"strings"
)

const (
	rateLimitBaseCooldownMs = 1000
	serverErrorCooldownMs   = 5000
	authErrorCooldownMs     = 300000
	maxBackoffLevel         = 5
)

// AuthError represents an authentication/authorization error with an HTTP status code.
type AuthError struct {
	Status int
	Msg    string
}

// Error implements the error interface.
func (e *AuthError) Error() string {
	return fmt.Sprintf("auth: status=%d msg=%s", e.Status, e.Msg)
}

var (
	ErrKeyDisabled = &AuthError{Status: http.StatusForbidden, Msg: "API key is disabled"}
	ErrKeyExpired  = &AuthError{Status: http.StatusForbidden, Msg: "API key has expired"}
)

// NoAvailableAccountError indicates no accounts are available for selection.
type NoAvailableAccountError struct {
	Provider string
}

// Error implements the error interface.
func (e *NoAvailableAccountError) Error() string {
	return fmt.Sprintf("no available accounts for provider %q", e.Provider)
}

// ErrorDecision describes how to react to an upstream error for an account.
type ErrorDecision struct {
	ShouldFallback  bool
	CooldownMs      int
	NewBackoffLevel int
}

// ClassifyError returns an ErrorDecision based on the HTTP status code and message.
// The message is inspected for provider-specific patterns (quota exhaustion,
// context length, etc.) to fine-tune the cooldown.
func ClassifyError(status int, message string, currentBackoffLevel int) ErrorDecision {
	msg := strings.ToLower(message)

	switch {
	case status == http.StatusTooManyRequests:
		newLevel := currentBackoffLevel + 1
		if newLevel > maxBackoffLevel {
			newLevel = maxBackoffLevel
		}
		cooldown := rateLimitBaseCooldownMs << (newLevel - 1)
		if strings.Contains(msg, "insufficient_quota") ||
			strings.Contains(msg, "billing") ||
			strings.Contains(msg, "payment") {
			cooldown = authErrorCooldownMs
		}
		return ErrorDecision{
			ShouldFallback:  true,
			CooldownMs:      cooldown,
			NewBackoffLevel: newLevel,
		}
	case status >= 500:
		return ErrorDecision{
			ShouldFallback:  true,
			CooldownMs:      serverErrorCooldownMs,
			NewBackoffLevel: currentBackoffLevel,
		}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ErrorDecision{
			ShouldFallback:  true,
			CooldownMs:      authErrorCooldownMs,
			NewBackoffLevel: currentBackoffLevel,
		}
	default:
		return ErrorDecision{
			ShouldFallback:  false,
			CooldownMs:      0,
			NewBackoffLevel: currentBackoffLevel,
		}
	}
}
