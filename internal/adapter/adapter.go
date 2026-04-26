package adapter

import (
	"fmt"
	"net/http"
)

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

func (a *BearerAuth) InjectAuth(req *http.Request, apiKey string) error {
	if apiKey == "" {
		return fmt.Errorf("bearer auth: API key is empty")
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return nil
}

// BearerPlusGoogAuth injects an API key via both Authorization and
// x-goog-api-key headers, used by Google AI providers.
type BearerPlusGoogAuth struct{}

func (a *BearerPlusGoogAuth) InjectAuth(req *http.Request, apiKey string) error {
	if apiKey == "" {
		return fmt.Errorf("bearer+goog auth: API key is empty")
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-goog-api-key", apiKey)
	return nil
}

type NoAuthAdapter struct {
	Caps Capabilities
}

func NewNoAuthAdapter(caps Capabilities) *NoAuthAdapter {
	return &NoAuthAdapter{Caps: caps}
}

func (a *NoAuthAdapter) InjectAuth(req *http.Request, apiKey string) error {
	return nil
}

func (a *NoAuthAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return (&OpenAIAdapter{Caps: a.Caps}).MutateRequest(body, model, stream)
}

func (a *NoAuthAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

func (a *NoAuthAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}

type NoAuth struct{}

func (a *NoAuth) InjectAuth(req *http.Request, apiKey string) error {
	return nil
}

type IdentityResponse struct{}

func (r *IdentityResponse) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}
