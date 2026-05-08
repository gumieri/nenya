package adapter

import (
	"net/http"
)

// AzureAdapter handles request/response mutation for Azure OpenAI API.
type AzureAdapter struct {
	Caps Capabilities
}

// NewAzureAdapter creates a new AzureAdapter with default capabilities.
func NewAzureAdapter() *AzureAdapter {
	return &AzureAdapter{
		Caps: Capabilities{
			StreamOptions:  false,
			AutoToolChoice: true,
			ContentArrays:  true,
		},
	}
}

// MutateRequest mutates the request body for Azure-specific requirements.
func (a *AzureAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return (&OpenAIAdapter{Caps: a.Caps}).MutateRequest(body, model, stream)
}

// InjectAuth adds the api-key header for Azure authentication.
func (a *AzureAdapter) InjectAuth(req *http.Request, apiKey string) error {
	if err := verifyAPIKey(apiKey, "azure"); err != nil {
		return err
	}
	req.Header.Set("api-key", apiKey)
	return nil
}

// MutateResponse returns the response body unchanged.
func (a *AzureAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

// NormalizeError classifies Azure HTTP errors into retryable, rate-limited, quota-exhausted, or permanent.
func (a *AzureAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}
