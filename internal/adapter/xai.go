package adapter

import (
	"net/http"
)

// XAIAdapter handles request/response mutation for xAI (Grok) API.
type XAIAdapter struct {
	Caps Capabilities
}

// NewXAIAdapter creates a new XAIAdapter with default capabilities.
func NewXAIAdapter() *XAIAdapter {
	return &XAIAdapter{
		Caps: Capabilities{
			StreamOptions:  true,
			AutoToolChoice: true,
			ContentArrays:  true,
		},
	}
}

// MutateRequest mutates the request body for xAI-specific requirements.
func (a *XAIAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return (&OpenAIAdapter{Caps: a.Caps}).MutateRequest(body, model, stream)
}

// InjectAuth adds the Bearer Authorization header to the request.
func (a *XAIAdapter) InjectAuth(req *http.Request, apiKey string) error {
	return (&BearerAuth{}).InjectAuth(req, apiKey)
}

// MutateResponse returns the response body unchanged.
func (a *XAIAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

// NormalizeError classifies xAI HTTP errors into retryable, rate-limited, quota-exhausted, or permanent.
func (a *XAIAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}
