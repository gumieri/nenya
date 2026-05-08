package pipeline

import (
	"strings"

	"nenya/config"
)

const (
	thoughtOpenTag  = "<think"
	thoughtCloseTag = "</think"
	thoughtMarker   = "[Reasoning pruned by gateway]"
)

// PruneThoughts removes <think ...>...</think > reasoning blocks from
// assistant message content to reduce token usage while preserving the final
// model output.
//
// It does NOT remove the structured reasoning_content field from assistant
// messages — that is provider-specific data handled by
// routing.SanitizePayload per-target so that providers requiring it
// (e.g. DeepSeek v4 thinking mode) receive it intact.
func PruneThoughts(payload map[string]interface{}, cfg config.CompactionConfig) bool {
	if cfg.PruneThoughts == nil || !*cfg.PruneThoughts {
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

	mutated := false

	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}

		if pruneThoughtTags(msg) {
			mutated = true
		}
	}

	return mutated
}

// StripReasoningContent removes the reasoning_content field from all
// assistant messages in the payload. This should be called per-target
// in SanitizePayload for providers that do not support reasoning, to avoid
// sending unknown fields upstream.
func StripReasoningContent(payload map[string]interface{}) bool {
	messagesRaw, ok := payload["messages"]
	if !ok {
		return false
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok || len(messages) == 0 {
		return false
	}

	mutated := false

	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}

		rc, ok := msg["reasoning_content"]
		if !ok {
			continue
		}

		str, ok := rc.(string)
		if !ok {
			delete(msg, "reasoning_content")
			mutated = true
			continue
		}
		if str == "" {
			continue
		}

		delete(msg, "reasoning_content")
		mutated = true
	}

	return mutated
}

func pruneThoughtTags(msg map[string]interface{}) bool {
	content, ok := msg["content"]
	if !ok {
		return false
	}

	str, ok := content.(string)
	if !ok {
		return false
	}

	pruned := stripThoughtBlocks(str)
	if pruned == str {
		return false
	}

	msg["content"] = pruned
	return true
}

func stripThoughtBlocks(s string) string {
	if !strings.Contains(s, thoughtOpenTag) {
		return s
	}

	var b strings.Builder

	rest := s
	for {
		idx := strings.Index(rest, thoughtOpenTag)
		if idx < 0 {
			if b.Len() == 0 {
				return s
			}
			b.WriteString(rest)
			break
		}

		if !isThinkTagBoundary(rest, idx+len(thoughtOpenTag)) {
			if b.Len() == 0 {
				return s
			}
			b.WriteString(rest[:idx+len(thoughtOpenTag)])
			rest = rest[idx+len(thoughtOpenTag):]
			continue
		}

		if b.Len() == 0 {
			b.Grow(len(rest))
		}

		b.WriteString(rest[:idx])

		afterOpen := rest[idx+len(thoughtOpenTag):]

		closeIdx := findCloseTag(afterOpen)

		if closeIdx < 0 {
			b.WriteString(thoughtMarker)
			break
		}

		b.WriteString(thoughtMarker)
		rest = afterOpen[closeIdx+len(thoughtCloseTag):]
	}

	result := b.String()
	if result == "" {
		return thoughtMarker
	}
	return result
}

func isThinkTagBoundary(s string, pos int) bool {
	if pos >= len(s) {
		return true
	}
	ch := s[pos]
	return ch == '>' || ch == '\n' || ch == '\r' || ch == ' ' || ch == '\t'
}

func findCloseTag(s string) int {
	maxIter := len(s)
	for i := 0; i < maxIter; i++ {
		idx := strings.Index(s, thoughtCloseTag)
		if idx < 0 {
			return -1
		}
		if isThinkTagBoundary(s, idx+len(thoughtCloseTag)) {
			return idx
		}
		s = s[idx+len(thoughtCloseTag):]
	}
	return -1
}
