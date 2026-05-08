package adapter

import (
	"net/http"
)

// DeepInfraAdapter handles request/response mutation for DeepInfra API.
type DeepInfraAdapter struct {
	Caps Capabilities
}

// NewDeepInfraAdapter creates a new DeepInfraAdapter with default capabilities.
func NewDeepInfraAdapter() *DeepInfraAdapter {
	return &DeepInfraAdapter{
		Caps: Capabilities{
			StreamOptions:  false,
			AutoToolChoice: true,
			ContentArrays:  true,
		},
	}
}

// MutateRequest mutates the request body for DeepInfra-specific requirements.
func (a *DeepInfraAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return (&OpenAIAdapter{Caps: a.Caps}).MutateRequest(body, model, stream)
}

// InjectAuth adds the Bearer Authorization header to the request.
func (a *DeepInfraAdapter) InjectAuth(req *http.Request, apiKey string) error {
	return (&BearerAuth{}).InjectAuth(req, apiKey)
}

// MutateResponse returns the response body unchanged.
func (a *DeepInfraAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

// NormalizeError classifies DeepInfra HTTP errors into retryable, rate-limited, quota-exhausted, or permanent.
func (a *DeepInfraAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}
