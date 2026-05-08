package adapter

import (
	"net/http"
)

// OpenRouterAdapter handles request/response mutation for OpenRouter API.
type OpenRouterAdapter struct {
	Caps Capabilities
}

// NewOpenRouterAdapter creates a new OpenRouterAdapter with default capabilities.
func NewOpenRouterAdapter() *OpenRouterAdapter {
	return &OpenRouterAdapter{
		Caps: Capabilities{
			StreamOptions:  true,
			AutoToolChoice: true,
			ContentArrays:  true,
		},
	}
}

// MutateRequest mutates the request body for OpenRouter-specific requirements.
func (a *OpenRouterAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return (&OpenAIAdapter{Caps: a.Caps}).MutateRequest(body, model, stream)
}

// InjectAuth adds the Bearer Authorization header and OpenRouter-specific headers to the request.
func (a *OpenRouterAdapter) InjectAuth(req *http.Request, apiKey string) error {
	if err := (&BearerAuth{}).InjectAuth(req, apiKey); err != nil {
		return err
	}
	req.Header.Set("HTTP-Referer", "https://github.com/nenya-project/nenya")
	req.Header.Set("X-Title", "Nenya")
	return nil
}

// MutateResponse returns the response body unchanged.
func (a *OpenRouterAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

// NormalizeError classifies OpenRouter HTTP errors into retryable, rate-limited, quota-exhausted, or permanent.
func (a *OpenRouterAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}
