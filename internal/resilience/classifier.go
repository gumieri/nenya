package resilience

import (
	"strings"
	"time"

	"github.com/nenya/internal/util"
)

// ErrorClass represents the semantic category of an error response.
type ErrorClass string

const (
	// ErrorClassAuth indicates authentication/authorization failures (e.g., invalid credentials).
	ErrorClassAuth ErrorClass = "auth"
	// ErrorClassRate indicates rate limiting errors.
	ErrorClassRate ErrorClass = "rate_limit"
	// ErrorClassQuota indicates quota exceeded or insufficient credit.
	ErrorClassQuota ErrorClass = "quota"
	// ErrorClassCapacity indicates server overload or capacity issues.
	ErrorClassCapacity ErrorClass = "capacity"
	// ErrorClassServer indicates generic server errors (5xx).
	ErrorClassServer ErrorClass = "server"
	// ErrorClassContextLimit indicates context length exceeded errors.
	ErrorClassContextLimit ErrorClass = "context_limit"
	// ErrorClassModelNotFound indicates the requested model is not available on
	// this provider (NENYA-168). It is a *target* fault, not a malformed client
	// request: the model may simply be absent from this provider while a later
	// target serves it, so the target is cooled down and the caller fails over.
	ErrorClassModelNotFound ErrorClass = "model_not_found"
	// ErrorClassUnknown indicates unclassified errors.
	ErrorClassUnknown ErrorClass = "unknown"
)

// CooldownDecision represents the circuit breaker's response to an error.
// The classifier returns a CooldownDecision for each upstream failure. The
// circuit breaker uses ShouldLock to decide whether to mark the model as
// temporarily unavailable and IncrementBackoff to decide whether to advance
// the exponential backoff level.
type CooldownDecision struct {
	// Class is the semantic error category (auth, rate_limit, quota, etc.).
	Class ErrorClass
	// ShouldLock indicates whether the model should be locked for the Cooldown duration.
	ShouldLock bool
	// Cooldown is the duration the model should be locked. For backoff-enabled classes
	// (rate_limit, quota, capacity), this includes exponential growth with jitter.
	Cooldown time.Duration
	// IncrementBackoff indicates whether to advance the backoff tracker for this key.
	// Only set for classes that use exponential backoff (rate_limit, quota, capacity).
	IncrementBackoff bool
}

// classifyHTTPError analyzes an HTTP status code and response body to determine
// the semantic error class, appropriate cooldown, and whether to lock the model
// and/or increment the backoff level. It uses the backoffLevel to compute
// exponential cooldowns with jitter for rate_limit, quota, and capacity errors.
func classifyHTTPError(status int, body string, backoffLevel int) CooldownDecision {
	// body is bounded by the caller (truncated before reaching here),
	// so the ToLower allocation is proportional to the bounded size.
	lower := strings.ToLower(body)

	switch {
	case util.IsModelNotFoundStatus(status, body):
		// Model-not-found from an upstream (NENYA-168) is checked first: the
		// marker is more specific than the generic 400/401/403/422 classes, and
		// it is a *target* fault — cool the target down (and lock the model on
		// it) so the sweep moves to a provider that has the model, instead of
		// relaying a client-class error.
		return CooldownDecision{
			Class:      ErrorClassModelNotFound,
			ShouldLock: true,
			Cooldown:   modelNotFoundCooldown,
		}

	case status == 401 || status == 403:
		return CooldownDecision{
			Class:      ErrorClassAuth,
			ShouldLock: true,
			Cooldown:   authCooldown,
		}

	case status == 429:
		cooldown := computeExponentialBackoffWithJitter(backoffLevel, rateLimitBaseMs)
		return CooldownDecision{
			Class:            ErrorClassRate,
			ShouldLock:       true,
			Cooldown:         cooldown,
			IncrementBackoff: true,
		}

	case status == 400 || status == 402:
		if strings.Contains(lower, "quota") || strings.Contains(lower, "insufficient") {
			cooldown := computeExponentialBackoffWithJitter(backoffLevel, quotaBaseMs)
			return CooldownDecision{
				Class:            ErrorClassQuota,
				ShouldLock:       true,
				Cooldown:         cooldown,
				IncrementBackoff: true,
			}
		}
		if util.IsContextLengthError(status, body) {
			return CooldownDecision{
				Class:      ErrorClassContextLimit,
				ShouldLock: false,
				Cooldown:   0,
			}
		}
		return CooldownDecision{
			Class:      ErrorClassUnknown,
			ShouldLock: false,
			Cooldown:   5 * time.Second,
		}

	case status == 413 || status == 422:
		if util.IsContextLengthError(status, body) {
			return CooldownDecision{
				Class:      ErrorClassContextLimit,
				ShouldLock: false,
				Cooldown:   0,
			}
		}
		return CooldownDecision{
			Class:      ErrorClassUnknown,
			ShouldLock: false,
			Cooldown:   5 * time.Second,
		}

	case status >= 500:
		if util.IsOverloadMessage(lower) {
			cooldown := computeExponentialBackoffWithJitter(backoffLevel, capacityBaseMs)
			return CooldownDecision{
				Class:            ErrorClassCapacity,
				ShouldLock:       true,
				Cooldown:         cooldown,
				IncrementBackoff: true,
			}
		}
		return CooldownDecision{
			Class:      ErrorClassServer,
			ShouldLock: true,
			Cooldown:   serverCooldown,
		}
	}

	return CooldownDecision{
		Class:      ErrorClassUnknown,
		ShouldLock: true,
		Cooldown:   unknownCooldown,
	}
}
