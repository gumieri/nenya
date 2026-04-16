package adapter

import (
	"net/http"
)

type XAIAdapter struct {
	Caps Capabilities
}

func NewXAIAdapter() *XAIAdapter {
	return &XAIAdapter{
		Caps: Capabilities{
			StreamOptions:  true,
			AutoToolChoice: true,
			ContentArrays:  true,
		},
	}
}

func (a *XAIAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return (&OpenAIAdapter{Caps: a.Caps}).MutateRequest(body, model, stream)
}

func (a *XAIAdapter) InjectAuth(req *http.Request, apiKey string) error {
	return (&BearerAuth{}).InjectAuth(req, apiKey)
}

func (a *XAIAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

func (a *XAIAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}
