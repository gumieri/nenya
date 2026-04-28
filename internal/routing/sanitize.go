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
func stripStreamOptions(deps TransformDeps, payload map[string]interface{}, providerName string) {
	if _, ok := payload["stream_options"]; !ok {
		return
	}
	if providerpkg.SupportsStreamOptions(providerName) {
		return
	}
	delete(payload, "stream_options")
	deps.Logger.Debug("stripped stream_options for provider", "provider", providerName)
}

func stripToolChoice(deps TransformDeps, payload map[string]interface{}, providerName string) {
	toolChoice, ok := payload["tool_choice"]
	if !ok {
		return
	}
	tc, ok := toolChoice.(string)
	if !ok || tc != "auto" {
		return
	}
	if providerpkg.SupportsAutoToolChoice(providerName) {
		return
	}
	delete(payload, "tool_choice")
	deps.Logger.Debug("stripped tool_choice \"auto\" for provider", "provider", providerName)
}

func flattenContentArrays(deps TransformDeps, payload map[string]interface{}, providerName string) {
	messagesRaw, ok := payload["messages"]
	if !ok {
		return
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok {
		return
	}
	if providerpkg.SupportsContentArrays(providerName) {
		return
	}
	changed := false
	for i, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		contentRaw, ok := msg["content"]
		if !ok {
			continue
		}
		arr, ok := contentRaw.([]interface{})
		if !ok || len(arr) == 0 {
			continue
		}
		flat := flattenContentArray(arr)
		if flat == "" {
			continue
		}
		messages[i].(map[string]interface{})["content"] = flat
		changed = true
	}
	if changed {
		deps.Logger.Debug("flattened content arrays for provider", "provider", providerName)
	}
}

func shouldStripReasoning(deps TransformDeps, providerName, modelName string) bool {
	if providerpkg.SupportsReasoning(providerName) {
		return false
	}
	if providerName == "deepseek" {
		return false
	}
	if deps.Catalog == nil || modelName == "" {
		return false
	}
	dm, ok := deps.Catalog.Lookup(modelName)
	if !ok {
		return false
	}
	if dm.Metadata == nil {
		return false
	}
	return !dm.Metadata.SupportsReasoning
}

func processReasoningContent(deps TransformDeps, payload map[string]interface{}, providerName, modelName string) {
	if !shouldStripReasoning(deps, providerName, modelName) {
		return
	}
	if pipeline.StripReasoningContent(payload) {
		deps.Logger.Debug("stripped reasoning_content",
			"provider", providerName, "model", modelName)
	}
}

func processMessages(deps TransformDeps, payload map[string]interface{}, providerName, modelName string) {
	messagesRaw, ok := payload["messages"]
	if !ok {
		return
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok {
		return
	}
	flattenContentArrays(deps, payload, providerName)
	processReasoningContent(deps, payload, providerName, modelName)
	repaired, repairedMessages := repairMessageOrdering(messages)
	if !repaired {
		return
	}
	payload["messages"] = repairedMessages
	deps.Logger.Warn("repaired invalid message role ordering", "provider", providerName)
}

func applyDeepSeekFixes(deps TransformDeps, payload map[string]interface{}, providerName string) {
	if providerName != "deepseek" {
		return
	}
	ensureDeepSeekReasoningContent(payload, deps.Logger)
	stripDeepSeekThinkingParams(payload, providerName, deps.Logger)
}

func SanitizePayload(deps TransformDeps, payload map[string]interface{}, providerName string, modelName string) {
	stripStreamOptions(deps, payload, providerName)
	stripToolChoice(deps, payload, providerName)
	processMessages(deps, payload, providerName, modelName)
	applyDeepSeekFixes(deps, payload, providerName)
}

// ensureDeepSeekReasoningContent injects an empty reasoning_content field
// on all assistant messages that lack it. DeepSeek v4 requires all assistant
// messages to carry reasoning_content in multi-turn conversations; requests
// without it return 400 errors with "reasoning_content must be passed back".
func ensureDeepSeekReasoningContent(payload map[string]interface{}, logger *slog.Logger) {
	messagesRaw, ok := payload["messages"]
	if !ok {
		return
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok {
		return
	}
	injected := 0
	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok || role != "assistant" {
			continue
		}
		if _, exists := msg["reasoning_content"]; !exists {
			msg["reasoning_content"] = ""
			injected++
		}
	}
	if injected > 0 {
		logger.Debug("injected reasoning_content on assistant messages", "count", injected)
	}
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
