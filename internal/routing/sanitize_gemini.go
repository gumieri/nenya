package routing

import (
	"nenya/internal/pipeline"
)

func SanitizeToolMessagesForGemini(deps TransformDeps, payload map[string]interface{}) {
	messagesRaw, ok := payload["messages"]
	if !ok {
		return
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok {
		return
	}

	type toolCallInfo struct {
		id       string
		name     string
		hasExtra bool
	}

	toolCallMap := make(map[string]*toolCallInfo)

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
			_, hasExtra := tc["extra_content"]
			if !hasExtra {
				if deps.ThoughtSigCache != nil {
					if cached, found := deps.ThoughtSigCache.Load(tcID); found {
						tc["extra_content"] = cached
						deps.Logger.Debug("gemini: injected cached thought_signature", "tool_call_id", tcID)
						hasExtra = true
					}
				}
			}
			var fnName string
			if fn, ok := tc["function"].(map[string]interface{}); ok {
				fnName, _ = fn["name"].(string)
			}
			toolCallMap[tcID] = &toolCallInfo{
				id:       tcID,
				name:     fnName,
				hasExtra: hasExtra,
			}
		}
	}

	orphanedIDs := make(map[string]bool)
	for tcID, info := range toolCallMap {
		if !info.hasExtra {
			orphanedIDs[tcID] = true
			deps.Logger.Warn("gemini: tool_call missing thought_signature, will strip pair",
				"tool_call_id", tcID)
		}
	}

	if len(orphanedIDs) == 0 {
		for _, msgRaw := range messages {
			msg, ok := msgRaw.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			if role != "tool" {
				continue
			}
			toolCallID, _ := msg["tool_call_id"].(string)
			if toolCallID == "" {
				continue
			}
			if _, hasName := msg["name"]; hasName {
				continue
			}
			if info, ok := toolCallMap[toolCallID]; ok && info.name != "" {
				msg["name"] = info.name
				deps.Logger.Debug("gemini: injected function name on tool message", "tool_call_id", toolCallID, "name", info.name)
			}
		}
		return
	}

	filtered := make([]interface{}, 0, len(messages))
	for i, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			filtered = append(filtered, msgRaw)
			continue
		}
		role, _ := msg["role"].(string)

		if role == "assistant" {
			toolCallsRaw, hasTC := msg["tool_calls"]
			if hasTC {
				toolCalls, ok := toolCallsRaw.([]interface{})
				if ok {
					cleaned := make([]interface{}, 0, len(toolCalls))
					for _, tcRaw := range toolCalls {
						tc, ok := tcRaw.(map[string]interface{})
						if !ok {
							cleaned = append(cleaned, tcRaw)
							continue
						}
						tcID, _ := tc["id"].(string)
						if orphanedIDs[tcID] {
							continue
						}
						cleaned = append(cleaned, tcRaw)
					}
					if len(cleaned) == 0 {
						content := pipeline.ExtractContentText(msg)
						if content == "" {
							deps.Logger.Debug("gemini: removed empty assistant message after stripping orphaned tool_calls", "index", i)
							continue
						}
						delete(msg, "tool_calls")
					} else {
						msg["tool_calls"] = cleaned
					}
				}
			}
			filtered = append(filtered, msgRaw)
			continue
		}

		if role == "tool" {
			toolCallID, _ := msg["tool_call_id"].(string)
			if toolCallID != "" && orphanedIDs[toolCallID] {
				deps.Logger.Debug("gemini: removed orphaned tool response", "tool_call_id", toolCallID)
				continue
			}

			if _, hasName := msg["name"]; !hasName {
				if info, ok := toolCallMap[toolCallID]; ok && info.name != "" {
					msg["name"] = info.name
					deps.Logger.Debug("gemini: injected function name on tool message",
						"tool_call_id", toolCallID, "name", info.name)
				} else {
					msg["name"] = "unknown_function"
					deps.Logger.Warn("gemini: assigned synthetic name to tool message",
						"tool_call_id", toolCallID)
				}
			}

			filtered = append(filtered, msgRaw)
			continue
		}

		filtered = append(filtered, msgRaw)
	}

	if len(filtered) != len(messages) {
		payload["messages"] = filtered
	}
}


