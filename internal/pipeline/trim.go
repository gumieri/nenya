package pipeline

import (
	"log/slog"

	"nenya/config"
)

func TrimPayload(logger *slog.Logger, payload map[string]interface{}, maxTokens int, countTokens func(string) int, cfg config.ContextConfig) (bool, int) {
	messagesRaw, ok := payload["messages"]
	if !ok {
		return false, 0
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok || len(messages) == 0 {
		return false, 0
	}

	tokenCount := countAllTokens(messages, countTokens)
	if tokenCount <= maxTokens {
		return false, 0
	}

	if logger != nil {
		logger.Info("trimming payload to fit token budget",
			"original_tokens", tokenCount,
			"max_tokens", maxTokens)
	}

	systemMessages, nonSystemMessages := partitionByRole(messages)

	kept := make([]interface{}, 0, len(nonSystemMessages))
	keptTokens := 0

	for i := len(nonSystemMessages) - 1; i >= 0; i-- {
		msgRaw := nonSystemMessages[i]
		msgTokens := tokenForMessage(msgRaw, countTokens)
		if keptTokens+msgTokens <= maxTokens {
			kept = append(kept, msgRaw)
			keptTokens += msgTokens
		} else if keptTokens < maxTokens {
			remaining := maxTokens - keptTokens
			truncated := truncateMessageByTokens(msgRaw, remaining, countTokens, cfg)
			kept = append(kept, truncated)
			keptTokens = keptTokens + tokenForMessage(truncated, countTokens)
			if logger != nil {
				logger.Info("truncated single message to fit budget",
					"original_tokens", msgTokens,
					"truncated_tokens", tokenForMessage(truncated, countTokens),
					"remaining", remaining)
			}
			break
		}
	}

	result := make([]interface{}, 0, len(systemMessages)+len(kept))
	result = append(result, systemMessages...)
	for i := len(kept) - 1; i >= 0; i-- {
		result = append(result, kept[i])
	}

	payload["messages"] = result

	newTokenCount := countAllTokens(result, countTokens)
	savedTokens := tokenCount - newTokenCount
	if logger != nil {
		logger.Info("payload trimmed",
			"original_tokens", tokenCount,
			"new_tokens", newTokenCount,
			"saved_tokens", savedTokens)
	}

	return true, savedTokens
}

func countAllTokens(messages []interface{}, countTokens func(string) int) int {
	total := 0
	for _, msgRaw := range messages {
		total += tokenForMessage(msgRaw, countTokens)
	}
	return total
}

func tokenForMessage(msgRaw interface{}, countTokens func(string) int) int {
	msg, ok := msgRaw.(map[string]interface{})
	if !ok {
		return 0
	}
	content, ok := msg["content"].(string)
	if !ok {
		return 0
	}
	return countTokens(content)
}

func partitionByRole(messages []interface{}) (system, nonSystem []interface{}) {
	system = make([]interface{}, 0)
	nonSystem = make([]interface{}, 0, len(messages))
	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			nonSystem = append(nonSystem, msgRaw)
			continue
		}
			role, ok := msg["role"].(string)
		if !ok || role != "system" {
			nonSystem = append(nonSystem, msgRaw)
		continue
		}
		system = append(system, msgRaw)
	}
	return system, nonSystem
}

func truncateMessageByTokens(msgRaw interface{}, maxTokens int, countTokens func(string) int, cfg config.ContextConfig) interface{} {
	msg, ok := msgRaw.(map[string]interface{})
	if !ok {
		return msgRaw
	}
	content, ok := msg["content"].(string)
	if !ok {
		return msgRaw
	}
	curTokens := countTokens(content)
	if curTokens <= maxTokens {
		return msgRaw
	}
	truncated := TruncateMiddleOutByTokens(content, maxTokens, countTokens, cfg)
	return map[string]interface{}{
		"role":    msg["role"],
		"content": truncated,
	}
}
