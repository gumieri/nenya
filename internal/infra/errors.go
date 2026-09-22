package infra

// ErrorKind categorizes errors for client diagnostics and retry decisions.
type ErrorKind string

const (
	ErrorKindNone            ErrorKind = ""
	ErrorKindContextExceeded ErrorKind = "context_exceeded"
	ErrorKindRateLimited     ErrorKind = "rate_limited"
	ErrorKindAuthFailed      ErrorKind = "auth_failed"
	ErrorKindModelNotFound   ErrorKind = "model_not_found"
	ErrorKindProviderTimeout ErrorKind = "provider_timeout"
	ErrorKindProviderError   ErrorKind = "provider_error"
	ErrorKindNetworkError    ErrorKind = "network_error"
	ErrorKindPayloadTooLarge ErrorKind = "payload_too_large"
	ErrorKindInvalidRequest  ErrorKind = "invalid_request"
	ErrorKindBouncerError    ErrorKind = "bouncer_error"
	ErrorKindInternal        ErrorKind = "internal_error"
	ErrorKindQuotaExhausted  ErrorKind = "quota_exhausted"
	ErrorKindInjection       ErrorKind = "injection_detected"
	ErrorKindExfil           ErrorKind = "exfil_blocked"
)

// ErrorResponse represents a structured API error.
type ErrorResponse struct {
	Error   ErrorBody `json:"error"`
	Kind    ErrorKind `json:"error_kind,omitempty"`
	Request string    `json:"request_id,omitempty"`
}

// ErrorBody holds the human-readable error details.
type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

// ErrorScope classifies who is at fault for an error. It is the taxonomy
// that resilience wiring is intended to drive (NENYA-42): request-scoped
// faults (the client's payload) must never mutate cooldown/rotation state;
// account-scoped faults (quota, bad credentials, rate limits) are the
// credential's problem (today enforced at target-level cooldown granularity);
// connection-scoped faults are transport-lifecycle events (client cancel,
// network) and must not poison state. Providers invent new request-fault
// shapes faster than defaults can track — the operator-configurable
// providers.<name>.request_scoped_errors rules override this taxonomy per
// provider.
type ErrorScope string

const (
	// ScopeRequest marks errors caused by the client's payload.
	ScopeRequest ErrorScope = "request"
	// ScopeAccount marks errors tied to the credential/account.
	ScopeAccount ErrorScope = "account"
	// ScopeConnection marks transport-lifecycle errors (cancel, network).
	ScopeConnection ErrorScope = "connection"
	// ScopeProvider marks upstream-side failures.
	ScopeProvider ErrorScope = "provider"
	// ScopeUnknown marks errors that carry no scope signal.
	ScopeUnknown ErrorScope = ""
)

// Scope returns the default fault scope for this error kind. Operator
// configuration (providers.<name>.request_scoped_errors) can override the
// classification per provider, because providers invent new request-fault
// shapes faster than the defaults can track.
func (k ErrorKind) Scope() ErrorScope {
	switch k {
	case ErrorKindInvalidRequest,
		ErrorKindPayloadTooLarge,
		ErrorKindModelNotFound,
		ErrorKindContextExceeded,
		ErrorKindInjection,
		ErrorKindExfil:
		return ScopeRequest
	case ErrorKindAuthFailed,
		ErrorKindQuotaExhausted,
		ErrorKindRateLimited:
		return ScopeAccount
	case ErrorKindNetworkError,
		ErrorKindProviderTimeout:
		return ScopeConnection
	case ErrorKindProviderError,
		ErrorKindBouncerError,
		ErrorKindInternal:
		return ScopeProvider
	default:
		return ScopeUnknown
	}
}

// Retryable returns true if the error is potentially retryable. Quota exhaustion,
// rate limits, provider timeouts, and network errors are considered retryable.
func (k ErrorKind) Retryable() bool {
	switch k {
	case ErrorKindRateLimited,
		ErrorKindQuotaExhausted,
		ErrorKindProviderTimeout,
		ErrorKindNetworkError:
		return true
	default:
		return false
	}
}

// ShouldFailover returns true if this error should trigger provider failover.
// Provider timeouts, provider errors, and network errors trigger failover.
// Quota exhaustion and rate limits do NOT trigger failover (all targets quota-limited).
func (k ErrorKind) ShouldFailover() bool {
	switch k {
	case ErrorKindProviderTimeout,
		ErrorKindProviderError,
		ErrorKindNetworkError:
		return true
	default:
		return false
	}
}
