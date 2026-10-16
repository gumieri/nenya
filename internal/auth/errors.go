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

// Account-selection failure reasons reported via NoAvailableAccountError.Reason.
const (
	// ReasonNotFound means the account ID is not present in the pool.
	ReasonNotFound = "not_found"
	// ReasonInactive means the account exists but its status is not active.
	ReasonInactive = "inactive"
	// ReasonCooling means the account is rate-limited (cooldown active).
	ReasonCooling = "cooling"
	// ReasonModelLocked means the account is model-locked for the model.
	ReasonModelLocked = "model_locked"
)

// NoAvailableAccountError indicates no accounts are available for selection.
// Reason distinguishes the failure mode (see the Reason* constants; empty for
// the LRU selection path) so sticky pin misses are diagnosable in production
// logs.
type NoAvailableAccountError struct {
	Provider string
	Reason   string
}

// Error implements the error interface.
func (e *NoAvailableAccountError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("no available accounts for provider %q (reason: %s)", e.Provider, e.Reason)
	}
	return fmt.Sprintf("no available accounts for provider %q", e.Provider)
}

// ErrorDecision describes how to react to an upstream error for an account.
type ErrorDecision struct {
	ShouldFallback  bool
	CooldownMs      int
	NewBackoffLevel int
}

// isModelScopedQuotaSignal reports whether an upstream error body scopes
// its quota/rate limit to a specific MODEL rather than the credential
// (NENYA-170): such a signal must bench only that model, not the
// account's other models. Deliberately tight — per-model quota field
// spellings: "per-model"/"permodel" (Gemini's
// GenerateRequestsPerDayPerProjectPerModel family), "model_name", a
// `"model":` JSON field, or the model id itself — the latter two only
// alongside a rate/quota context word, since aggregators echo the model
// as request metadata for ACCOUNT-scoped limits. An unrecognized body
// fails safe to the account scope. The signal is only as trustworthy as
// the provider: a crafted body can keep a dead credential un-benched.
func isModelScopedQuotaSignal(message, model string) bool {
	msg := strings.ToLower(message)
	if strings.Contains(msg, "per-model") || strings.Contains(msg, "permodel") {
		return true
	}
	if strings.Contains(msg, "model_name") {
		return true
	}
	rateContext := strings.Contains(msg, "quota") || strings.Contains(msg, "limit") ||
		strings.Contains(msg, "rate") || strings.Contains(msg, "requests")
	if !rateContext {
		return false
	}
	if strings.Contains(msg, `"model":`) {
		return true
	}
	return model != "" && strings.Contains(msg, "model") && strings.Contains(msg, strings.ToLower(model))
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
