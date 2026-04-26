package routing

import (
	"log/slog"
	"strings"

	"nenya/internal/pipeline"
	providerpkg "nenya/internal/providers"
)

// SanitizePayload removes unsupported fields from the request payload
// based on the target provider's capabilities (e.g. stream_options,
// tool_choice, reasoning parameters). It also validates and repairs
// message role ordering to comply with the OpenAI chat completions spec.
func SanitizePayload(deps TransformDeps, payload map[string]interface{}, providerName string, modelName string) {
	if _, ok := payload["stream_options"]; ok {
		if !providerpkg.SupportsStreamOptions(providerName) {
			delete(payload, "stream_options")
			deps.Logger.Debug("stripped stream_options for provider", "provider", providerName)
		}
	}

	if toolChoice, ok := payload["tool_choice"]; ok {
		if tc, ok := toolChoice.(string); ok && tc == "auto" {
			if !providerpkg.SupportsAutoToolChoice(providerName) {
				delete(payload, "tool_choice")
				deps.Logger.Debug("stripped tool_choice \"auto\" for provider", "provider", providerName)
			}
		}
	}

	if messagesRaw, ok := payload["messages"]; ok {
		if messages, ok := messagesRaw.([]interface{}); ok {
			if !providerpkg.SupportsContentArrays(providerName) {
				changed := false
				for i, msgRaw := range messages {
					msg, ok := msgRaw.(map[string]interface{})
					if !ok {
						continue
					}
					if contentRaw, ok := msg["content"]; ok {
						if arr, ok := contentRaw.([]interface{}); ok && len(arr) > 0 {
							if flat := flattenContentArray(arr); flat != "" {
								messages[i].(map[string]interface{})["content"] = flat
								changed = true
							}
						}
					}
				}
				if changed {
					deps.Logger.Debug("flattened content arrays for provider", "provider", providerName)
				}
			}

			shouldStripReasoning := !providerpkg.SupportsReasoning(providerName)
			if !shouldStripReasoning && deps.Catalog != nil && modelName != "" {
				if dm, ok := deps.Catalog.Lookup(modelName); !ok || dm.Metadata == nil || !dm.Metadata.SupportsReasoning {
					shouldStripReasoning = true
				}
			}
			if shouldStripReasoning {
				if pipeline.StripReasoningContent(payload) {
					deps.Logger.Debug("stripped reasoning_content",
						"provider", providerName, "model", modelName)
				}
			}

			repaired, repairedMessages := repairMessageOrdering(messages)
			if repaired {
				payload["messages"] = repairedMessages
				deps.Logger.Warn("repaired invalid message role ordering", "provider", providerName)
			}
		}
	}

	stripDeepSeekThinkingParams(payload, providerName, deps.Logger)
}

func stripDeepSeekThinkingParams(payload map[string]interface{}, providerName string, logger *slog.Logger) {
	if providerName != "deepseek" {
		return
	}
	thinking, ok := payload["thinking"]
	if !ok {
		return
	}
	tm, ok := thinking.(map[string]interface{})
	if !ok {
		return
	}
	typ, ok := tm["type"].(string)
	if !ok || typ != "enabled" {
		return
	}
	for _, key := range []string{"temperature", "top_p", "presence_penalty", "frequency_penalty"} {
		if _, has := payload[key]; has {
			delete(payload, key)
			logger.Debug("stripped param ignored in thinking mode", "param", key, "provider", providerName)
		}
	}
}

func flattenContentArray(arr []interface{}) string {
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
	return strings.Join(parts, "\n")
}

// repairMessageOrdering validates and repairs message role ordering to comply
// with the OpenAI chat completions API spec. The valid role sequence requires
// that a "user" message never directly follows a "tool" message — there must
// be an "assistant" message in between. When this invariant is violated, an
// assistant message is inserted to bridge the gap.
//
// Returns true if any repairs were made, and the repaired messages slice.
func repairMessageOrdering(messages []interface{}) (bool, []interface{}) {
	if len(messages) < 2 {
		return false, messages
	}

	repaired := false
	sawToolSinceLastAssistant := false

	bridgeMsg := map[string]interface{}{
		"role":    "assistant",
		"content": "",
	}

	i := 0
	for i < len(messages) {
		msg, ok := messages[i].(map[string]interface{})
		if !ok {
			i++
			continue
		}

		role, ok := msg["role"].(string)
		if !ok {
			i++
			continue
		}

		switch {
		case role == "tool":
			sawToolSinceLastAssistant = true
			i++

		case role == "user" && sawToolSinceLastAssistant:
			messages = append(messages[:i], append([]interface{}{bridgeMsg}, messages[i:]...)...)
			repaired = true
			sawToolSinceLastAssistant = false
			i += 2

		default:
			sawToolSinceLastAssistant = false
			i++
		}
	}

	return repaired, messages
}
