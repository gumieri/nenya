package adapter

import (
	"fmt"
	"net/http"
)

// verifyAPIKey returns an error if the API key is empty.
func verifyAPIKey(apiKey, provider string) error {
	if apiKey == "" {
		return fmt.Errorf("%s auth: API key is empty", provider)
	}
	return nil
}

// ErrorClass classifies upstream errors for retry/circuit-breaker decisions.
type ErrorClass int

const (
	ErrorPermanent ErrorClass = iota
	ErrorRetryable
	ErrorRateLimited
	ErrorQuotaExhausted
)

func (e ErrorClass) String() string {
	switch e {
	case ErrorPermanent:
		return "permanent"
	case ErrorRetryable:
		return "retryable"
	case ErrorRateLimited:
		return "rate_limited"
	case ErrorQuotaExhausted:
		return "quota_exhausted"
	default:
		return "unknown"
	}
}

// Capabilities declares which optional features a provider supports.
type Capabilities struct {
	StreamOptions  bool
	AutoToolChoice bool
	ContentArrays  bool
}

// ProviderAdapter defines the interface for provider-specific request/response
// mutation, authentication injection, and error classification.
type ProviderAdapter interface {
	MutateRequest(body []byte, model string, stream bool) ([]byte, error)
	InjectAuth(req *http.Request, apiKey string) error
	MutateResponse(body []byte) ([]byte, error)
	NormalizeError(statusCode int, body []byte) ErrorClass
}

// BearerAuth injects an API key via the standard Authorization header.
type BearerAuth struct{}

// InjectAuth adds the Bearer Authorization header to the request.
func (a *BearerAuth) InjectAuth(req *http.Request, apiKey string) error {
	if err := verifyAPIKey(apiKey, "bearer"); err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return nil
}

// BearerPlusGoogAuth injects an API key via both Authorization and
// x-goog-api-key headers, used by Google AI providers.
type BearerPlusGoogAuth struct{}

// InjectAuth adds the Bearer Authorization header and the x-goog-api-key header to the request.
func (a *BearerPlusGoogAuth) InjectAuth(req *http.Request, apiKey string) error {
	if err := verifyAPIKey(apiKey, "bearer+goog"); err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-goog-api-key", apiKey)
	return nil
}

// NoAuthAdapter is an adapter for providers that do not require authentication.
type NoAuthAdapter struct {
	Caps Capabilities
}

// NewNoAuthAdapter creates a new NoAuthAdapter with the given capabilities.
func NewNoAuthAdapter(caps Capabilities) *NoAuthAdapter {
	return &NoAuthAdapter{Caps: caps}
}

// InjectAuth is a no-op for providers that do not require authentication.
func (a *NoAuthAdapter) InjectAuth(req *http.Request, apiKey string) error {
	return nil
}

// MutateRequest delegates to the OpenAI adapter for request mutation.
func (a *NoAuthAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return (&OpenAIAdapter{Caps: a.Caps}).MutateRequest(body, model, stream)
}

// MutateResponse returns the response body unchanged.
func (a *NoAuthAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

// NormalizeError classifies errors using the default normalization logic.
func (a *NoAuthAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}

// NoAuth is a marker type for providers that do not require authentication.
type NoAuth struct{}

// InjectAuth is a no-op for providers that do not require authentication.
func (a *NoAuth) InjectAuth(req *http.Request, apiKey string) error {
	return nil
}

// IdentityResponse is a response transformer that returns the body unchanged.
type IdentityResponse struct{}

// MutateResponse returns the response body unchanged.
func (r *IdentityResponse) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}
