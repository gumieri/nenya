package adapter

import (
	"net/http"
)

// CohereAdapter handles request/response mutation for Cohere API.
type CohereAdapter struct {
	Caps Capabilities
}

// NewCohereAdapter creates a new CohereAdapter with default capabilities.
func NewCohereAdapter() *CohereAdapter {
	return &CohereAdapter{
		Caps: Capabilities{
			StreamOptions:  false,
			AutoToolChoice: true,
			ContentArrays:  false,
		},
	}
}

// MutateRequest mutates the request body for Cohere-specific requirements.
func (a *CohereAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return (&OpenAIAdapter{Caps: a.Caps}).MutateRequest(body, model, stream)
}

// InjectAuth adds the Bearer Authorization header to the request.
func (a *CohereAdapter) InjectAuth(req *http.Request, apiKey string) error {
	return (&BearerAuth{}).InjectAuth(req, apiKey)
}

// MutateResponse returns the response body unchanged.
func (a *CohereAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

// NormalizeError classifies Cohere HTTP errors into retryable, rate-limited, quota-exhausted, or permanent.
func (a *CohereAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}
