package adapter

import (
	"net/http"
)

type DeepInfraAdapter struct {
	Caps Capabilities
}

func NewDeepInfraAdapter() *DeepInfraAdapter {
	return &DeepInfraAdapter{
		Caps: Capabilities{
			StreamOptions:  false,
			AutoToolChoice: true,
			ContentArrays:  true,
		},
	}
}

func (a *DeepInfraAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return (&OpenAIAdapter{Caps: a.Caps}).MutateRequest(body, model, stream)
}

func (a *DeepInfraAdapter) InjectAuth(req *http.Request, apiKey string) error {
	return (&BearerAuth{}).InjectAuth(req, apiKey)
}

func (a *DeepInfraAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

func (a *DeepInfraAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}
