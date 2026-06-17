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

// Retryable returns true if the error is potentially retryable.
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
