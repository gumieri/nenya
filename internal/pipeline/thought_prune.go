package pipeline

import (
	"strings"

	"nenya/internal/config"
)

const (
	thoughtOpenTag  = "<think"
	thoughtCloseTag = "</think"
	thoughtMarker   = "[Reasoning pruned by gateway]"
)

func PruneThoughts(payload map[string]interface{}, cfg config.CompactionConfig) bool {
	if !cfg.PruneThoughts {
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

		if pruneReasoningField(msg) {
			mutated = true
		}

		if pruneThoughtTags(msg) {
			mutated = true
		}
	}

	return mutated
}

func pruneReasoningField(msg map[string]interface{}) bool {
	rc, ok := msg["reasoning_content"]
	if !ok {
		return false
	}

	str, ok := rc.(string)
	if !ok {
		delete(msg, "reasoning_content")
		return true
	}
	if str == "" {
		return false
	}

	delete(msg, "reasoning_content")
	return true
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
	b.Grow(len(s))

	rest := s
	for {
		idx := strings.Index(rest, thoughtOpenTag)
		if idx < 0 {
			b.WriteString(rest)
			break
		}

		b.WriteString(rest[:idx])

		afterOpen := rest[idx+len(thoughtOpenTag):]

		closeIdx := strings.Index(afterOpen, thoughtCloseTag)

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
