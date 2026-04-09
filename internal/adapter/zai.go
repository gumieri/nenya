package adapter

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type ZAIAdapter struct {
	extractContent func(msg map[string]interface{}) string
	logger         zaiLogger
}

type zaiLogger interface {
	Debug(msg string, args ...any)
}

type ZAIAdapterDeps struct {
	ExtractContent func(msg map[string]interface{}) string
	Logger         zaiLogger
}

func NewZAIAdapter(deps ZAIAdapterDeps) *ZAIAdapter {
	return &ZAIAdapter{
		extractContent: deps.ExtractContent,
		logger:         deps.Logger,
	}
}

func (a *ZAIAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}

	if a.extractContent == nil {
		return body, nil
	}

	if a.zaiSanitize(payload) {
		out, err := json.Marshal(payload)
		if err != nil {
			return body, fmt.Errorf("zai: failed to marshal mutated request: %w", err)
		}
		return out, nil
	}

	return body, nil
}

func (a *ZAIAdapter) InjectAuth(req *http.Request, apiKey string) error {
	return (&BearerAuth{}).InjectAuth(req, apiKey)
}

func (a *ZAIAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

func (a *ZAIAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}

func (a *ZAIAdapter) zaiSanitize(payload map[string]interface{}) bool {
	messagesRaw, ok := payload["messages"]
	if !ok {
		return false
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok {
		return false
	}

	validToolCallIDs := make(map[string]string)
	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		toolCallsRaw, ok := msg["tool_calls"]
		if !ok {
			continue
		}
		toolCalls, ok := toolCallsRaw.([]interface{})
		if !ok {
			continue
		}
		for _, tcRaw := range toolCalls {
			tc, ok := tcRaw.(map[string]interface{})
			if !ok {
				continue
			}
			tcID, _ := tc["id"].(string)
			if tcID == "" {
				continue
			}
			var fnName string
			if fn, ok := tc["function"].(map[string]interface{}); ok {
				fnName, _ = fn["name"].(string)
			}
			validToolCallIDs[tcID] = fnName
		}
	}

	filtered := make([]interface{}, 0, len(messages))
	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			filtered = append(filtered, msgRaw)
			continue
		}
		role, _ := msg["role"].(string)
		content := a.extractContent(msg)

		if content == "" && role != "tool" && role != "assistant" && role != "system" {
			continue
		}

		if role == "assistant" && content == "" {
			if tcRaw, hasTC := msg["tool_calls"]; !hasTC {
				continue
			} else {
				toolCalls, ok := tcRaw.([]interface{})
				if !ok || len(toolCalls) == 0 {
					continue
				}
			}
		}

		if role == "tool" {
			toolCallID, _ := msg["tool_call_id"].(string)
			if toolCallID == "" {
				if a.logger != nil {
					a.logger.Debug("zai: removing tool message without tool_call_id")
				}
				continue
			}
			if _, ok := validToolCallIDs[toolCallID]; !ok {
				if a.logger != nil {
					a.logger.Debug("zai: removing orphaned tool message", "tool_call_id", toolCallID)
				}
				continue
			}
		}

		filtered = append(filtered, msgRaw)
	}

	if len(filtered) == 0 {
		return false
	}

	merged := make([]interface{}, 0, len(filtered))
	for i, msgRaw := range filtered {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			merged = append(merged, msgRaw)
			continue
		}
		role, _ := msg["role"].(string)

		if i > 0 {
			prevMsg, ok := merged[len(merged)-1].(map[string]interface{})
			if ok {
				prevRole, _ := prevMsg["role"].(string)
				if prevRole == role && role == "user" {
					prevContent := a.extractContent(prevMsg)
					currContent := a.extractContent(msg)
					prevMsg["content"] = prevContent + "\n\n" + currContent
					continue
				}
				if prevRole == "assistant" && role == "assistant" {
					merged = append(merged, map[string]interface{}{
						"role":    "user",
						"content": "Continue.",
					})
					if a.logger != nil {
						a.logger.Debug("zai: inserted user bridge between consecutive assistant messages")
					}
				}
			}
		}

		merged = append(merged, msgRaw)
	}

	if len(merged) > 0 {
		if firstMsg, ok := merged[0].(map[string]interface{}); ok {
			if role, _ := firstMsg["role"].(string); role == "user" {
				bridgeMsg := map[string]interface{}{
					"role":    "system",
					"content": "Continue the conversation.",
				}
				merged = append([]interface{}{bridgeMsg}, merged...)
				if a.logger != nil {
					a.logger.Debug("zai: prepended system bridge before leading user message")
				}
			}
		}
	}

	if len(merged) != len(messages) {
		if a.logger != nil {
			a.logger.Debug("zai: sanitized message sequence",
				"messages_before", len(messages), "messages_after", len(merged))
		}
	}

	payload["messages"] = merged
	return true
}

func init() {
	var _ ProviderAdapter = (*ZAIAdapter)(nil)
}
