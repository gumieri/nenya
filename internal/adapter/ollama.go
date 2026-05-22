package adapter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

// OllamaAdapter handles request/response mutation for Ollama (local) API.
type OllamaAdapter struct{}

// NewOllamaAdapter creates a new OllamaAdapter instance.
func NewOllamaAdapter() *OllamaAdapter {
	return &OllamaAdapter{}
}

// stripToolChoice removes the "tool_choice" field from a JSON object if present.
// Returns the modified map and a boolean indicating whether any change was made.
func stripToolChoice(body map[string]any) (map[string]any, bool) {
	if _, exists := body["tool_choice"]; exists {
		delete(body, "tool_choice")
		return body, true
	}
	return body, false
}

// MutateRequest removes the "tool_choice" field from the request body.
// The /v1/chat/completions endpoint does not support this field.
func (a *OllamaAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}

	stripped, changed := stripToolChoice(payload)
	if !changed {
		return body, nil
	}

	modified, err := json.Marshal(stripped)
	if err != nil {
		slog.Warn("failed to marshal stripped Ollama request", "error", err)
		return body, fmt.Errorf("failed to marshal request after stripping tool_choice: %w", err)
	}

	slog.Debug("stripped unsupported tool_choice field from Ollama request",
		"model", model, "stream", stream)

	return modified, nil
}

// InjectAuth optionally adds the Bearer Authorization header if an API key is present.
func (a *OllamaAdapter) InjectAuth(req *http.Request, apiKey string) error {
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return nil
}

// MutateResponse returns the response body unchanged.
func (a *OllamaAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

// NormalizeError classifies Ollama HTTP errors into retryable, rate-limited, quota-exhausted, or permanent.
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
