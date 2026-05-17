package providers

import (
	"net/url"
	"strings"
)

func zaiSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		SanitizeRequest:    zaiSanitize,
		ValidationEndpoint: zaiValidationEndpoint,
	}
}

func zaiSanitize(deps *SanitizeDeps, payload map[string]interface{}) {
	injectThinkingForZai(deps, payload)
	injectTemperatureDefaultsForZai(payload)

	if _, hasTools := payload["tools"]; hasTools {
		return
	}

	messagesRaw, ok := payload["messages"]
	if !ok {
		return
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok {
		return
	}

	validToolCallIDs := zaiExtractValidToolCallIDs(messages)
	filtered := zaiFilterMessages(deps, messages, validToolCallIDs)

	if len(filtered) == 0 {
		return
	}

	merged := zaiMergeConsecutiveMessages(deps, filtered)
	merged = zaiPrependSystemBridge(deps, merged)

	if len(merged) != len(messages) {
		deps.Logger.Debug("zai: sanitized message sequence",
			"messages_before", len(messages), "messages_after", len(merged))
	}

	payload["messages"] = merged
}

func zaiExtractValidToolCallIDs(messages []interface{}) map[string]string {
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

			fnName := zaiExtractFunctionName(tc)
			validToolCallIDs[tcID] = fnName
		}
	}

	return validToolCallIDs
}

func zaiExtractFunctionName(tc map[string]interface{}) string {
	fn, ok := tc["function"].(map[string]interface{})
	if !ok {
		return ""
	}
	fnName, _ := fn["name"].(string)
	return fnName
}

func zaiFilterMessages(deps *SanitizeDeps, messages []interface{}, validToolCallIDs map[string]string) []interface{} {
	filtered := make([]interface{}, 0, len(messages))

	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			filtered = append(filtered, msgRaw)
			continue
		}
		role, _ := msg["role"].(string)
		content := deps.ExtractContentText(msg)

		if zaiShouldSkipMessage(role, content, msg) {
			continue
		}

		if zaiShouldSkipToolMessage(deps, role, msg, validToolCallIDs) {
			continue
		}

		filtered = append(filtered, msgRaw)
	}

	return filtered
}

func zaiShouldSkipMessage(role, content string, msg map[string]interface{}) bool {
	if content == "" && role != "tool" && role != "assistant" && role != "system" {
		return true
	}

	if role != "assistant" || content != "" {
		return false
	}

	tcRaw, hasTC := msg["tool_calls"]
	if !hasTC {
		return true
	}
	toolCalls, ok := tcRaw.([]interface{})
	return !ok || len(toolCalls) == 0
}

func zaiShouldSkipToolMessage(deps *SanitizeDeps, role string, msg map[string]interface{}, validToolCallIDs map[string]string) bool {
	if role != "tool" {
		return false
	}

	toolCallID, _ := msg["tool_call_id"].(string)
	if toolCallID == "" {
		deps.Logger.Debug("zai: removing tool message without tool_call_id")
		return true
	}

	if _, ok := validToolCallIDs[toolCallID]; !ok {
		deps.Logger.Debug("zai: removing orphaned tool message", "tool_call_id", toolCallID)
		return true
	}

	return false
}

func zaiMergeConsecutiveMessages(deps *SanitizeDeps, filtered []interface{}) []interface{} {
	merged := make([]interface{}, 0, len(filtered))

	for i, msgRaw := range filtered {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			merged = append(merged, msgRaw)
			continue
		}
		role, _ := msg["role"].(string)

		if i > 0 {
			if shouldMerge, shouldBridge, _ := checkMessageMerge(merged, msg, role); shouldMerge {
				continue
			} else if shouldBridge {
				merged = append(merged, map[string]interface{}{
					"role":    "user",
					"content": "Continue.",
				})
				deps.Logger.Debug("zai: inserted user bridge between consecutive assistant messages")
			}
		}

		merged = append(merged, msgRaw)
	}

	return merged
}

func checkMessageMerge(merged []interface{}, msg map[string]interface{}, role string) (shouldMerge, shouldBridge bool, prevMsg map[string]interface{}) {
	prevMsg, ok := merged[len(merged)-1].(map[string]interface{})
	if !ok {
		return false, false, nil
	}
	if zaiShouldMergeUserMessages(prevMsg, msg, role) {
		return true, false, prevMsg
	}
	if zaiShouldInsertAssistantBridge(prevMsg, role) {
		return false, true, prevMsg
	}
	return false, false, prevMsg
}

func zaiShouldMergeUserMessages(prevMsg, msg map[string]interface{}, role string) bool {
	prevRole, _ := prevMsg["role"].(string)
	if prevRole == role && role == "user" {
		prevContent := prevMsg["content"].(string)
		currContent := msg["content"].(string)
		prevMsg["content"] = prevContent + "\n\n" + currContent
		return true
	}
	return false
}

func zaiShouldInsertAssistantBridge(prevMsg map[string]interface{}, role string) bool {
	prevRole, _ := prevMsg["role"].(string)
	return prevRole == "assistant" && role == "assistant"
}

func zaiPrependSystemBridge(deps *SanitizeDeps, merged []interface{}) []interface{} {
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
	deps.Logger.Debug("zai: prepended system bridge before leading user message")

	return merged
}

// injectThinkingForZai enables thinking mode for Zai models that support reasoning.
// Three modes are supported:
//   - Auto (nil config): enabled when model's capabilities indicate reasoning support
//   - Force enabled (config.Thinking.Enabled=true): always injects thinking
//   - Force disabled (config.Thinking.Enabled=false): skips injection
func injectThinkingForZai(deps *SanitizeDeps, payload map[string]interface{}) {
	model, _ := payload["model"].(string)
	if model == "" {
		return
	}

	if deps.ProviderThinking != nil {
		if enabled, clearThinking, ok := deps.ProviderThinking("zai"); ok {
			if !enabled {
				return
			}
			payload["thinking"] = map[string]interface{}{
				"type":           "enabled",
				"clear_thinking": clearThinking,
			}
			logThinkingEnabled(deps.Logger, model, "force-enabled")
			return
		}
	}

	if deps.SupportsReasoning == nil {
		return
	}
	if !deps.SupportsReasoning(model) {
		return
	}
	if _, hasThinking := payload["thinking"]; hasThinking {
		return
	}

	payload["thinking"] = map[string]interface{}{
		"type":           "enabled",
		"clear_thinking": false,
	}
	logThinkingEnabled(deps.Logger, model, "auto-enabled")
}

func logThinkingEnabled(logger interface{ Debug(string, ...interface{}) }, model, mode string) {
	if logger == nil {
		return
	}
	logger.Debug("zai: "+mode+" thinking mode for reasoning model", "model", model)
}

// injectTemperatureDefaultsForZai sets model-specific temperature defaults.
// GLM-4.6 and GLM-4.7 require temperature=1.0 for optimal output.
func injectTemperatureDefaultsForZai(payload map[string]interface{}) {
	model, _ := payload["model"].(string)
	if model == "" {
		return
	}
	modelLower := strings.ToLower(model)
	if strings.Contains(modelLower, "glm-4.6") || strings.Contains(modelLower, "glm-4.7") {
		if _, hasTemp := payload["temperature"]; !hasTemp {
			payload["temperature"] = 1.0
		}
	}
}

func zaiValidationEndpoint(providerURL string) string {
	u, err := url.Parse(providerURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Host)

	if strings.Contains(host, "api.z.ai") {
		return "https://api.z.ai/v1/models"
	}
	return defaultValidationEndpoint(providerURL, u.Path)
}
