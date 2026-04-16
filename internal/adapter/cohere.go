package adapter

import (
	"net/http"
)

type CohereAdapter struct {
	Caps Capabilities
}

func NewCohereAdapter() *CohereAdapter {
	return &CohereAdapter{
		Caps: Capabilities{
			StreamOptions:  false,
			AutoToolChoice: true,
			ContentArrays:  false,
		},
	}
}

func (a *CohereAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return (&OpenAIAdapter{Caps: a.Caps}).MutateRequest(body, model, stream)
}

func (a *CohereAdapter) InjectAuth(req *http.Request, apiKey string) error {
	return (&BearerAuth{}).InjectAuth(req, apiKey)
}

func (a *CohereAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

func (a *CohereAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}
