package adapter

import (
	"net/http"
)

// MistralAdapter handles request/response mutation for Mistral AI API.
type MistralAdapter struct {
	Caps Capabilities
}

// NewMistralAdapter creates a new MistralAdapter with default capabilities.
func NewMistralAdapter() *MistralAdapter {
	return &MistralAdapter{
		Caps: Capabilities{
			StreamOptions:  false,
			AutoToolChoice: true,
			ContentArrays:  true,
		},
	}
}

// MutateRequest mutates the request body for Mistral-specific requirements.
func (a *MistralAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return (&OpenAIAdapter{Caps: a.Caps}).MutateRequest(body, model, stream)
}

// InjectAuth adds the Bearer Authorization header to the request.
func (a *MistralAdapter) InjectAuth(req *http.Request, apiKey string) error {
	return (&BearerAuth{}).InjectAuth(req, apiKey)
}

// MutateResponse returns the response body unchanged.
func (a *MistralAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

// NormalizeError classifies Mistral HTTP errors into retryable, rate-limited, quota-exhausted, or permanent.
func (a *MistralAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}
