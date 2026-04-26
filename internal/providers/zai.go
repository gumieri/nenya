package providers

import (
	"net/url"
	"strings"
)

func zaiSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  true,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsReasoning:      true,
		SupportsVision:         false,
		SanitizeRequest:        zaiSanitize,
		ValidationEndpoint:     zaiValidationEndpoint,
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
		content := deps.ExtractContentText(msg)

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
				deps.Logger.Debug("zai: removing tool message without tool_call_id")
				continue
			}
			if _, ok := validToolCallIDs[toolCallID]; !ok {
				deps.Logger.Debug("zai: removing orphaned tool message", "tool_call_id", toolCallID)
				continue
			}
		}

		filtered = append(filtered, msgRaw)
	}

	if len(filtered) == 0 {
		return
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
					prevContent := deps.ExtractContentText(prevMsg)
					currContent := deps.ExtractContentText(msg)
					prevMsg["content"] = prevContent + "\n\n" + currContent
					continue
				}
				if prevRole == "assistant" && role == "assistant" {
					merged = append(merged, map[string]interface{}{
						"role":    "user",
						"content": "Continue.",
					})
					deps.Logger.Debug("zai: inserted user bridge between consecutive assistant messages")
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
				deps.Logger.Debug("zai: prepended system bridge before leading user message")
			}
		}
	}

	if len(merged) != len(messages) {
		deps.Logger.Debug("zai: sanitized message sequence",
			"messages_before", len(messages), "messages_after", len(merged))
	}

	payload["messages"] = merged
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
			if deps.Logger != nil {
				deps.Logger.Debug("zai: force-enabled thinking mode", "model", model)
			}
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
	if deps.Logger != nil {
		deps.Logger.Debug("zai: auto-enabled thinking mode for reasoning model", "model", model)
	}
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
