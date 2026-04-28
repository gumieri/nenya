package adapter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
	if len(body) > 0 {
		var errResp struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &errResp) == nil && errResp.Error.Code != "" {
			switch errResp.Error.Code {
			case "1302", "1303":
				return ErrorRateLimited
			case "1308", "1310":
				return ErrorQuotaExhausted
			case "1312":
				return ErrorRetryable
			case "1311", "1313":
				return ErrorPermanent
			}
		}
		lower := strings.ToLower(string(body))
		if strings.Contains(lower, "model_context_window_exceeded") {
			return ErrorRetryable
		}
	}
	return defaultNormalizeError(statusCode, body)
}

func (a *ZAIAdapter) zaiSanitize(payload map[string]interface{}) bool {
	if _, hasTools := payload["tools"]; hasTools {
		return false
	}

	messagesRaw, ok := payload["messages"]
	if !ok {
		return false
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok {
		return false
	}

	validIDs := a.collectValidToolCallIDs(messages)

	filtered := a.zaiFilterMessages(messages, validIDs)
	if len(filtered) == 0 {
		return false
	}

	merged := a.zaiMergeSequentialMessages(filtered)
	merged = a.zaiPrependBridgeIfNeeded(merged)

	if len(merged) != len(messages) {
		a.logDebug("zai: sanitized message sequence",
			"messages_before", len(messages), "messages_after", len(merged))
	}

	payload["messages"] = merged
	return true
}

func (a *ZAIAdapter) collectValidToolCallIDs(messages []interface{}) map[string]string {
	ids := make(map[string]string)
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
			ids[tcID] = fnName
		}
	}
	return ids
}

func (a *ZAIAdapter) zaiFilterMessages(messages []interface{}, validIDs map[string]string) []interface{} {
	filtered := make([]interface{}, 0, len(messages))
	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			filtered = append(filtered, msgRaw)
			continue
		}
		role, _ := msg["role"].(string)
		content := a.extractContent(msg)

		if a.shouldDropMessage(role, content, msg) {
			continue
		}

		if role == "tool" {
			if a.shouldDropToolMessage(msg, validIDs) {
				continue
			}
		}

		filtered = append(filtered, msgRaw)
	}
	return filtered
}

func (a *ZAIAdapter) shouldDropMessage(role, content string, msg map[string]interface{}) bool {
	if content == "" && role != "tool" && role != "assistant" && role != "system" {
		return true
	}

	if role == "assistant" && content == "" {
		return a.assistantHasNoToolCalls(msg)
	}

	return false
}

func (a *ZAIAdapter) assistantHasNoToolCalls(msg map[string]interface{}) bool {
	tcRaw, hasTC := msg["tool_calls"]
	if !hasTC {
		return true
	}
	toolCalls, ok := tcRaw.([]interface{})
	if !ok || len(toolCalls) == 0 {
		return true
	}
	return false
}

func (a *ZAIAdapter) shouldDropToolMessage(msg map[string]interface{}, validIDs map[string]string) bool {
	toolCallID, _ := msg["tool_call_id"].(string)
	if toolCallID == "" {
		a.logDebug("zai: removing tool message without tool_call_id")
		return true
	}
	if _, ok := validIDs[toolCallID]; !ok {
		a.logDebug("zai: removing orphaned tool message", "tool_call_id", toolCallID)
		return true
	}
	return false
}

func (a *ZAIAdapter) zaiMergeSequentialMessages(filtered []interface{}) []interface{} {
	merged := make([]interface{}, 0, len(filtered))
	for i, msgRaw := range filtered {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			merged = append(merged, msgRaw)
			continue
		}

		if i == 0 {
			merged = append(merged, msgRaw)
			continue
		}

		role, _ := msg["role"].(string)
		merged = a.mergeIntoLast(merged, msg, role, msgRaw)
	}
	return merged
}

func (a *ZAIAdapter) mergeIntoLast(merged []interface{}, msg map[string]interface{}, role string, msgRaw interface{}) []interface{} {
	prevMsg, ok := merged[len(merged)-1].(map[string]interface{})
	if !ok {
		return append(merged, msgRaw)
	}

	prevRole, _ := prevMsg["role"].(string)

	if prevRole == role && role == "user" {
		prevContent := a.extractContent(prevMsg)
		currContent := a.extractContent(msg)
		prevMsg["content"] = prevContent + "\n\n" + currContent
		return merged
	}

	if prevRole == "assistant" && role == "assistant" {
		merged = append(merged, map[string]interface{}{
			"role":    "user",
			"content": "Continue.",
		})
		a.logDebug("zai: inserted user bridge between consecutive assistant messages")
	}

	return append(merged, msgRaw)
}

func (a *ZAIAdapter) zaiPrependBridgeIfNeeded(merged []interface{}) []interface{} {
	if len(merged) == 0 {
		return merged
	}

	firstMsg, ok := merged[0].(map[string]interface{})
	if !ok {
		return merged
	}

	role, _ := firstMsg["role"].(string)
	if role != "user" {
		return merged
	}

	bridgeMsg := map[string]interface{}{
		"role":    "system",
		"content": "Continue the conversation.",
	}
	merged = append([]interface{}{bridgeMsg}, merged...)
	a.logDebug("zai: prepended system bridge before leading user message")
	return merged
}

func (a *ZAIAdapter) logDebug(msg string, args ...any) {
	if a.logger != nil {
		a.logger.Debug(msg, args...)
	}
}

func init() {
	var _ ProviderAdapter = (*ZAIAdapter)(nil)
}
