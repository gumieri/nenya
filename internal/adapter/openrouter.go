package adapter

import (
	"net/http"
)

type OpenRouterAdapter struct {
	Caps Capabilities
}

func NewOpenRouterAdapter() *OpenRouterAdapter {
	return &OpenRouterAdapter{
		Caps: Capabilities{
			StreamOptions:  true,
			AutoToolChoice: true,
			ContentArrays:  true,
		},
	}
}

func (a *OpenRouterAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return (&OpenAIAdapter{Caps: a.Caps}).MutateRequest(body, model, stream)
}

func (a *OpenRouterAdapter) InjectAuth(req *http.Request, apiKey string) error {
	if err := (&BearerAuth{}).InjectAuth(req, apiKey); err != nil {
		return err
	}
	req.Header.Set("HTTP-Referer", "https://github.com/nenya-project/nenya")
	req.Header.Set("X-Title", "Nenya")
	return nil
}

func (a *OpenRouterAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

func (a *OpenRouterAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}
