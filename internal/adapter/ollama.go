package adapter

import (
	"net/http"
)

type OllamaAdapter struct{}

func NewOllamaAdapter() *OllamaAdapter {
	return &OllamaAdapter{}
}

func (a *OllamaAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return body, nil
}

func (a *OllamaAdapter) InjectAuth(req *http.Request, apiKey string) error {
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return nil
}

func (a *OllamaAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

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
