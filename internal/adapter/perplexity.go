package adapter

import (
	"net/http"
)

type PerplexityAdapter struct {
	Caps Capabilities
}

func NewPerplexityAdapter() *PerplexityAdapter {
	return &PerplexityAdapter{
		Caps: Capabilities{
			StreamOptions:  false,
			AutoToolChoice: false,
			ContentArrays:  true,
		},
	}
}

func (a *PerplexityAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return (&OpenAIAdapter{Caps: a.Caps}).MutateRequest(body, model, stream)
}

func (a *PerplexityAdapter) InjectAuth(req *http.Request, apiKey string) error {
	return (&BearerAuth{}).InjectAuth(req, apiKey)
}

func (a *PerplexityAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

func (a *PerplexityAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}
