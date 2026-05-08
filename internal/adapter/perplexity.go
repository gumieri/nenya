package adapter

import (
	"net/http"
)

// PerplexityAdapter handles request/response mutation for Perplexity API.
type PerplexityAdapter struct {
	Caps Capabilities
}

// NewPerplexityAdapter creates a new PerplexityAdapter with default capabilities.
func NewPerplexityAdapter() *PerplexityAdapter {
	return &PerplexityAdapter{
		Caps: Capabilities{
			StreamOptions:  false,
			AutoToolChoice: false,
			ContentArrays:  true,
		},
	}
}

// MutateRequest mutates the request body for Perplexity-specific requirements.
func (a *PerplexityAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return (&OpenAIAdapter{Caps: a.Caps}).MutateRequest(body, model, stream)
}

// InjectAuth adds the Bearer Authorization header to the request.
func (a *PerplexityAdapter) InjectAuth(req *http.Request, apiKey string) error {
	return (&BearerAuth{}).InjectAuth(req, apiKey)
}

// MutateResponse returns the response body unchanged.
func (a *PerplexityAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

// NormalizeError classifies Perplexity HTTP errors into retryable, rate-limited, quota-exhausted, or permanent.
func (a *PerplexityAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}
