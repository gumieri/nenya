package pipeline

import (
	"fmt"

	"nenya/internal/config"
)

const defaultToolProtectionWindow = 4

// PruneStaleToolCalls removes old tool_call/tool_result message pairs
// from the conversation history, keeping only the most recent ones within
// the configured protection window.
func PruneStaleToolCalls(payload map[string]interface{}, cfg config.CompactionConfig) bool {
	if !cfg.PruneStaleTools {
		return false
	}

	messagesRaw, ok := payload["messages"]
	if !ok {
		return false
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok || len(messages) == 0 {
		return false
	}

	protectionWindow := cfg.ToolProtectionWindow
	if protectionWindow <= 0 {
		protectionWindow = defaultToolProtectionWindow
	}

	n := len(messages)
	if n <= protectionWindow {
		return false
	}

	mutated := false
	pruneEnd := n - protectionWindow

	i := pruneEnd - 1
	for i >= 0 {
		msg, ok := messages[i].(map[string]interface{})
		if !ok {
			i--
			continue
		}

		role, _ := msg["role"].(string)
		if role != "assistant" {
			i--
			continue
		}

		tcRaw, hasToolCalls := msg["tool_calls"]
		if !hasToolCalls {
			i--
			continue
		}

		tcSlice, ok := tcRaw.([]interface{})
		if !ok || len(tcSlice) == 0 {
			i--
			continue
		}

		var toolName string
		if tc, ok := tcSlice[0].(map[string]interface{}); ok {
			if fn, ok := tc["function"].(map[string]interface{}); ok {
				if name, ok := fn["name"].(string); ok {
					toolName = name
				}
			}
		}

		toolCallID := extractToolCallID(tcSlice[0])

		numToolCalls := len(tcSlice)
		if i+numToolCalls >= pruneEnd {
			i--
			continue
		}

		allPaired := true
		for j := 0; j < numToolCalls; j++ {
			respIdx := i + 1 + j
			if respIdx >= pruneEnd {
				allPaired = false
				break
			}
			respMsg, ok := messages[respIdx].(map[string]interface{})
			if !ok {
				allPaired = false
				break
			}
			respRole, _ := respMsg["role"].(string)
			if respRole != "tool" {
				allPaired = false
				break
			}
		}

		if !allPaired {
			i--
			continue
		}

		displayName := toolName
		if displayName == "" {
			displayName = toolCallID
		}
		if displayName == "" {
			displayName = "unknown"
		}

		summary := fmt.Sprintf(
			"[System] Tool '%s' was executed previously. Result compacted to save context window.",
			displayName,
		)

		replacement := map[string]interface{}{
			"role":    "assistant",
			"content": summary,
		}
		if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
			replacement["reasoning_content"] = rc
		}

		total := 1 + numToolCalls
		messages[i] = replacement
		copy(messages[i+1:], messages[i+total:])
		for k := 0; k < numToolCalls; k++ {
			messages[n-1-k] = nil
		}
		messages = messages[:n-numToolCalls]
		n = len(messages)
		pruneEnd = n - protectionWindow
		if pruneEnd < 0 {
			pruneEnd = 0
		}

		mutated = true
		i = pruneEnd - 1
	}

	if mutated {
		payload["messages"] = messages
	}

	return mutated
}

func extractToolCallID(tc interface{}) string {
	tcMap, ok := tc.(map[string]interface{})
	if !ok {
		return ""
	}
	id, _ := tcMap["id"].(string)
	return id
}
