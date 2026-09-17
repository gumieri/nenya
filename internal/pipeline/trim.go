package pipeline

import (
	"log/slog"

	"github.com/nenya/config"
	"github.com/nenya/internal/util"
)

// toolOutputClampRunes bounds a single tool result's content before the
// budget math runs (mirroring opencode's compaction clamp): an oversized
// tool output is shrunk in place — middle-out, tool_call_id and all other
// fields preserved — instead of forcing its whole exchange to be dropped.
const toolOutputClampRunes = 4096

// TrimPayload reduces the payload's message list until its token count
// is <= maxTokens. It works on trim units (a plain message, or an atomic
// assistant-tool_calls + tool-results exchange), keeping the most recent
// units whole and using TruncateMiddleOutByTokens when a single plain
// message must be shortened. Tool exchanges are never split: a tool
// result is always kept or dropped together with the assistant message
// that issued the call, so trimmed payloads remain valid multi-turn
// conversations. Returns modified status and number of tokens removed.
// No-op if maxTokens <= 0 (used to disable proactive truncation when
// MaxContext is unknown; upstream providers may still reject with
// context_length_exceeded errors).
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
	if tokenCount <= maxTokens || maxTokens <= 0 {
		return false, 0
	}

	if logger != nil {
		logger.Info("trimming payload to fit token budget",
			"original_tokens", tokenCount,
			"max_tokens", maxTokens)
	}

	systemMessages, nonSystemMessages := partitionByRole(messages)

	clampToolOutputs(nonSystemMessages, cfg)

	var keptUnits [][]interface{}
	keptTokens := 0

	units := groupTrimUnits(nonSystemMessages)
	for i := len(units) - 1; i >= 0; i-- {
		unit := units[i]
		unitTokens := 0
		for _, msgRaw := range unit {
			unitTokens += tokenForMessage(msgRaw, countTokens)
		}

		if keptTokens+unitTokens <= maxTokens {
			keptUnits = append(keptUnits, unit)
			keptTokens += unitTokens
			continue
		}

		// Budget boundary. Only a single plain message may be shortened
		// into the remaining budget; a tool exchange is dropped whole —
		// cutting one would orphan its tool results or its tool_calls.
		if keptTokens < maxTokens && len(unit) == 1 {
			remaining := maxTokens - keptTokens
			truncated := truncateMessageByTokens(unit[0], remaining, countTokens, cfg)
			keptUnits = append(keptUnits, []interface{}{truncated})
			if logger != nil {
				logger.Info("truncated single message to fit budget",
					"original_tokens", unitTokens,
					"truncated_tokens", tokenForMessage(truncated, countTokens),
					"remaining", remaining)
			}
		}
		break
	}

	result := make([]interface{}, 0, util.AddCap(len(systemMessages), len(nonSystemMessages)))
	result = append(result, systemMessages...)
	for i := len(keptUnits) - 1; i >= 0; i-- {
		result = append(result, keptUnits[i]...)
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

// groupTrimUnits partitions non-system messages into atomic trim units: an
// assistant message carrying tool_calls is grouped with every immediately
// following tool result, so dropping the unit never splits a tool exchange.
// Every other message is its own unit.
func groupTrimUnits(msgs []interface{}) [][]interface{} {
	units := make([][]interface{}, 0, len(msgs))
	for i := 0; i < len(msgs); i++ {
		msg, ok := msgs[i].(map[string]interface{})
		if !ok {
			units = append(units, msgs[i:i+1])
			continue
		}
		role, _ := msg["role"].(string)
		calls, _ := msg["tool_calls"].([]interface{})
		if role != "assistant" || len(calls) == 0 {
			units = append(units, msgs[i:i+1])
			continue
		}

		unit := []interface{}{msgs[i]}
		for i+1 < len(msgs) {
			next, ok := msgs[i+1].(map[string]interface{})
			if !ok {
				break
			}
			if nextRole, _ := next["role"].(string); nextRole != "tool" {
				break
			}
			unit = append(unit, msgs[i+1])
			i++
		}
		units = append(units, unit)
	}
	return units
}

// clampToolOutputs replaces oversized tool-result content with a
// middle-out truncated copy, keeping tool_call_id and every other field
// intact so pairing survives the clamp.
func clampToolOutputs(msgs []interface{}, cfg config.ContextConfig) {
	for i, msgRaw := range msgs {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "tool" {
			continue
		}
		content, ok := msg["content"].(string)
		if !ok || len([]rune(content)) <= toolOutputClampRunes {
			continue
		}
		out := make(map[string]interface{}, len(msg))
		for k, v := range msg {
			out[k] = v
		}
		out["content"] = TruncateMiddleOut(content, toolOutputClampRunes, cfg)
		msgs[i] = out
	}
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
	// Preserve every other field (tool_call_id, name, ...) so a truncated
	// message keeps its identity and pairing semantics.
	out := make(map[string]interface{}, len(msg))
	for k, v := range msg {
		out[k] = v
	}
	out["content"] = truncated
	return out
}
