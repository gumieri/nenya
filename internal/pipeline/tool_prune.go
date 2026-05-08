package pipeline

import (
	"fmt"

	"nenya/config"
)

const defaultToolProtectionWindow = 4

// PruneStaleToolCalls removes old tool_call/tool_result message pairs
// from the conversation history, keeping only the most recent ones within
// the configured protection window.
func PruneStaleToolCalls(payload map[string]interface{}, cfg config.CompactionConfig) bool {
	if cfg.PruneStaleTools == nil || !*cfg.PruneStaleTools {
		return false
	}

	messages, ok := getMessagesFromPayload(payload)
	if !ok {
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

	for i := pruneEnd - 1; i >= 0; {
		msg, ok := messages[i].(map[string]interface{})
		if !ok || shouldSkipMessage(msg) {
			i--
			continue
		}

		tcSlice, ok := msg["tool_calls"].([]interface{})
		if !ok || len(tcSlice) == 0 {
			i--
			continue
		}

		toolName, toolCallID := extractToolCallInfo(tcSlice[0])
		numToolCalls := len(tcSlice)
		if numToolCalls > 256 {
			i--
			continue
		}

		if i+numToolCalls >= pruneEnd {
			i--
			continue
		}

		if !areToolResultsPaired(messages, i, tcSlice, pruneEnd) {
			i--
			continue
		}

		replacement := createToolCallReplacement(msg, toolName, toolCallID)
		messages, n, pruneEnd = pruneToolCallResults(messages, i, numToolCalls, replacement, n, protectionWindow)

		mutated = true
		i = pruneEnd - 1
	}

	if mutated {
		payload["messages"] = messages
	}

	return mutated
}

func getMessagesFromPayload(payload map[string]interface{}) ([]interface{}, bool) {
	messagesRaw, ok := payload["messages"]
	if !ok {
		return nil, false
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok || len(messages) == 0 {
		return nil, false
	}
	return messages, true
}

func shouldSkipMessage(msg map[string]interface{}) bool {
	role, _ := msg["role"].(string)
	return role != "assistant"
}

func extractToolCallInfo(tc interface{}) (string, string) {
	var toolName, toolCallID string

	tcMap, ok := tc.(map[string]interface{})
	if !ok {
		return "", ""
	}

	if fn, ok := tcMap["function"].(map[string]interface{}); ok {
		toolName, _ = fn["name"].(string)
	}

	toolCallID, _ = tcMap["id"].(string)
	return toolName, toolCallID
}

func areToolResultsPaired(messages []interface{}, i int, tcSlice []interface{}, pruneEnd int) bool {
	for j, tc := range tcSlice {
		respIdx := i + 1 + j
		if respIdx >= pruneEnd {
			return false
		}
		respMsg, ok := messages[respIdx].(map[string]interface{})
		if !ok {
			return false
		}
		respRole, _ := respMsg["role"].(string)
		if respRole != "tool" {
			return false
		}
		if wantID := extractToolCallID(tc); wantID != "" {
			gotID, _ := respMsg["tool_call_id"].(string)
			if gotID != wantID {
				return false
			}
		}
	}
	return true
}

// createToolCallReplacement builds a compact placeholder message that replaces
// a pruned tool call and its results. Preserves reasoning_content if present.
func createToolCallReplacement(msg map[string]interface{}, toolName, toolCallID string) map[string]interface{} {
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
	return replacement
}

// pruneToolCallResults replaces a tool call and its response messages with a
// single compact summary message. Returns the updated messages slice, new length,
// and updated prune end index.
func pruneToolCallResults(messages []interface{}, i int, numToolCalls int, replacement map[string]interface{}, n int, protectionWindow int) ([]interface{}, int, int) {
	messages[i] = replacement
	total := 1 + numToolCalls
	copy(messages[i+1:], messages[i+total:])
	for k := 0; k < numToolCalls; k++ {
		messages[n-1-k] = nil
	}
	messages = messages[:n-numToolCalls]
	n = len(messages)
	pruneEnd := n - protectionWindow
	if pruneEnd < 0 {
		pruneEnd = 0
	}
	return messages, n, pruneEnd
}

func extractToolCallID(tc interface{}) string {
	tcMap, ok := tc.(map[string]interface{})
	if !ok {
		return ""
	}
	id, _ := tcMap["id"].(string)
	return id
}
