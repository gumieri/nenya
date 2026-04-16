package adapter

import (
	"net/http"
)

type MistralAdapter struct {
	Caps Capabilities
}

func NewMistralAdapter() *MistralAdapter {
	return &MistralAdapter{
		Caps: Capabilities{
			StreamOptions:  false,
			AutoToolChoice: true,
			ContentArrays:  true,
		},
	}
}

func (a *MistralAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return (&OpenAIAdapter{Caps: a.Caps}).MutateRequest(body, model, stream)
}

func (a *MistralAdapter) InjectAuth(req *http.Request, apiKey string) error {
	return (&BearerAuth{}).InjectAuth(req, apiKey)
}

func (a *MistralAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

func (a *MistralAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}
