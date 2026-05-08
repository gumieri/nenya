package adapter

import (
	"encoding/json"
	"net/http"
	"strings"
)

// OpenAIAdapter handles request/response mutation for OpenAI-compatible APIs.
type OpenAIAdapter struct {
	Caps Capabilities
}

// NewOpenAIAdapter creates a new OpenAIAdapter with the given capabilities.
func NewOpenAIAdapter(caps Capabilities) *OpenAIAdapter {
	return &OpenAIAdapter{Caps: caps}
}

// MutateRequest mutates the request body based on the provider's capabilities.
func (a *OpenAIAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}

	changed := false

	if !a.Caps.StreamOptions {
		if _, has := payload["stream_options"]; has {
			delete(payload, "stream_options")
			changed = true
		}
	}

	if !a.Caps.AutoToolChoice {
		if tc, has := payload["tool_choice"]; has {
			if s, ok := tc.(string); ok && s == "auto" {
				delete(payload, "tool_choice")
				changed = true
			}
		}
	}

	if !a.Caps.ContentArrays {
		if a.flattenMessages(payload) {
			changed = true
		}
	}

	if !changed {
		return body, nil
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return body, nil
	}
	return out, nil
}

// flattenMessages flattens content arrays in messages into plain text.
func (a *OpenAIAdapter) flattenMessages(payload map[string]interface{}) bool {
	msgsRaw, has := payload["messages"]
	if !has {
		return false
	}
	msgs, ok := msgsRaw.([]interface{})
	if !ok {
		return false
	}
	return flattenContentArrays(msgs)
}

// InjectAuth adds the Bearer Authorization header to the request.
func (a *OpenAIAdapter) InjectAuth(req *http.Request, apiKey string) error {
	return (&BearerAuth{}).InjectAuth(req, apiKey)
}

// MutateResponse returns the response body unchanged.
func (a *OpenAIAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

// NormalizeError classifies OpenAI HTTP errors into retryable, rate-limited, quota-exhausted, or permanent.
func (a *OpenAIAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}

// flattenContentArrays flattens content arrays in messages into plain text.
func flattenContentArrays(msgs []interface{}) bool {
	changed := false
	for i, msgRaw := range msgs {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		contentRaw, has := msg["content"]
		if !has {
			continue
		}
		arr, ok := contentRaw.([]interface{})
		if !ok || len(arr) == 0 {
			continue
		}
		var parts []string
		for _, item := range arr {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if typ, ok := m["type"].(string); ok && typ == "text" {
				if text, ok := m["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		if len(parts) > 0 {
			msgs[i].(map[string]interface{})["content"] = strings.Join(parts, "\n")
			changed = true
		}
	}
	return changed
}

// defaultNormalizeError classifies HTTP errors into retryable, rate-limited, quota-exhausted, or permanent.
func defaultNormalizeError(statusCode int, body []byte) ErrorClass {
	switch statusCode {
	case 429:
		return ErrorRateLimited
	case 500, 502, 503, 504:
		return ErrorRetryable
	case 400, 413, 422:
		if len(body) == 0 {
			return ErrorPermanent
		}
		lower := strings.ToLower(string(body))
		for _, pat := range commonRetryablePatterns {
			if strings.Contains(lower, pat) {
				return ErrorRetryable
			}
		}
		return ErrorPermanent
	default:
		return ErrorPermanent
	}
}

var commonRetryablePatterns = []string{
	"unavailable_model",
	"tokens_limit_reached",
	"context_length_exceeded",
	"context length",
	"model_overloaded",
	"overloaded",
	"thought_signature",
	"name cannot be empty",
	"messages parameter is illegal",
	"unknown_model",
	"max_tokens",
	"rate_limit_exceeded",
	"extra_forbidden",
	"enable-auto-tool-choice",
	"tool_call_parser",
	"valid string",
}
