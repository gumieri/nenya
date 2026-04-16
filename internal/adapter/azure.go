package adapter

import (
	"fmt"
	"net/http"
)

var errEmptyAPIKey = fmt.Errorf("azure auth: API key is empty")

type AzureAdapter struct {
	Caps Capabilities
}

func NewAzureAdapter() *AzureAdapter {
	return &AzureAdapter{
		Caps: Capabilities{
			StreamOptions:  false,
			AutoToolChoice: true,
			ContentArrays:  true,
		},
	}
}

func (a *AzureAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return (&OpenAIAdapter{Caps: a.Caps}).MutateRequest(body, model, stream)
}

func (a *AzureAdapter) InjectAuth(req *http.Request, apiKey string) error {
	if apiKey == "" {
		return errEmptyAPIKey
	}
	req.Header.Set("api-key", apiKey)
	return nil
}

func (a *AzureAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

func (a *AzureAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}
