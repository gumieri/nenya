package adapter

import (
	"net/http"
)

// OllamaAdapter handles request/response mutation for Ollama (local) API.
type OllamaAdapter struct{}

// NewOllamaAdapter creates a new OllamaAdapter instance.
func NewOllamaAdapter() *OllamaAdapter {
	return &OllamaAdapter{}
}

// MutateRequest returns the request body unchanged.
func (a *OllamaAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return body, nil
}

// InjectAuth optionally adds the Bearer Authorization header if an API key is present.
func (a *OllamaAdapter) InjectAuth(req *http.Request, apiKey string) error {
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return nil
}

// MutateResponse returns the response body unchanged.
func (a *OllamaAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

// NormalizeError classifies Ollama HTTP errors into retryable, rate-limited, quota-exhausted, or permanent.
func (a *OllamaAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	switch statusCode {
	case 429:
		return ErrorRateLimited
	case 500, 502, 503, 504:
		return ErrorRetryable
	default:
		return ErrorPermanent
	}
}

func init() {
	var _ ProviderAdapter = (*OllamaAdapter)(nil)
}
