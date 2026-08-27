package adapter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ZAIAdapter handles request/response mutation for Zai (Zhipu AI) API.
type ZAIAdapter struct {
	extractContent func(msg map[string]interface{}) string
	logger         zaiLogger
}

// zaiLogger is an interface for structured logging in the Zai adapter.
type zaiLogger interface {
	Debug(msg string, args ...any)
}

// ZAIAdapterDeps contains dependencies for creating a ZAIAdapter.
type ZAIAdapterDeps struct {
	ExtractContent func(msg map[string]interface{}) string
	Logger         zaiLogger
}

// NewZAIAdapter creates a new ZAIAdapter with the given dependencies.
func NewZAIAdapter(deps ZAIAdapterDeps) *ZAIAdapter {
	return &ZAIAdapter{
		extractContent: deps.ExtractContent,
		logger:         deps.Logger,
	}
}

// MutateRequest mutates the request body for Zai-specific requirements.
// The model and stream parameters are part of the ProviderAdapter interface
// but are unused here; model-specific logic (thinking injection, temperature
// defaults) lives in the provider spec layer (internal/providers/zai.go).
// Note: this adapter layer is only exercised when an adapter is explicitly
// registered for the provider; the default production sanitization path is
// ZaiSanitizeAdapterOnly in internal/providers — keep the two in sync.
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

// InjectAuth adds the Bearer Authorization header to the request.
func (a *ZAIAdapter) InjectAuth(req *http.Request, apiKey string) error {
	return (&BearerAuth{}).InjectAuth(req, apiKey)
}

// MutateResponse returns the response body unchanged.
func (a *ZAIAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

// NormalizeError classifies Zai HTTP errors into retryable, rate-limited, quota-exhausted, or permanent.
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

	changed := len(filtered) != len(messages)

	merged := a.zaiMergeSequentialMessages(filtered)
	if len(merged) != len(filtered) {
		changed = true
	}
	lenBeforePrepend := len(merged)
	merged = a.zaiPrependBridgeIfNeeded(merged)
	if len(merged) != lenBeforePrepend {
		changed = true
	}

	// Change-detection contract: each stage above (zaiFilterMessages,
	// zaiMergeSequentialMessages, zaiPrependBridgeIfNeeded) only removes,
	// merges, or inserts whole messages. Merging may mutate the surviving
	// message's content in place, but that mutation always co-occurs with a
	// message-count change. Under this contract, no length change means the
	// sequence was already normalized. Skip the reassignment so MutateRequest
	// can return the original body untouched. A future stage that mutates
	// messages in place without a count change MUST extend this detection.
	if !changed {
		return false
	}

	a.logDebug("zai: sanitized message sequence",
		"messages_before", len(messages), "messages_after", len(merged))

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

// zaiFilterMessages drops messages per the z.ai requirements: empty
// user-style messages (empty content in roles other than tool, assistant, or
// system), assistant messages with empty content and no tool calls, and tool
// messages that are missing a tool_call_id or reference an unknown/orphaned
// tool call ID. It only removes messages; see the change-detection contract
// in zaiSanitize.
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

// zaiMergeSequentialMessages merges consecutive same-role messages into the
// earlier message, mutating that message's content in place and reducing the
// message count (see the change-detection contract in zaiSanitize).
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

// zaiPrependBridgeIfNeeded prepends a synthetic system bridge message when the
// sequence starts with a user message, increasing the message count by at most
// one (see the change-detection contract in zaiSanitize).
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
