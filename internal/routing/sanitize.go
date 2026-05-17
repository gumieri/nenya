package routing

import (
	"log/slog"
	"math"
	"strings"

	"nenya/internal/discovery"
	"nenya/internal/pipeline"
)

func modelSupportsCapability(deps TransformDeps, modelName string, cap discovery.Capability) bool {
	meta := resolveModelMeta(deps, modelName)
	if meta == nil {
		return false
	}
	return meta.HasCapability(cap)
}

func resolveModelMeta(deps TransformDeps, modelName string) *discovery.ModelMetadata {
	if deps.Catalog != nil && modelName != "" {
		if dm, ok := deps.Catalog.Lookup(modelName); ok && dm.Metadata != nil {
			return dm.Metadata
		}
	}
	return discovery.InferCapabilities(modelName)
}

// SanitizePayload removes unsupported fields from the request payload
// based on the target model's capabilities (e.g. stream_options,
// tool_choice, reasoning parameters). It also validates and repairs
// message role ordering to comply with the OpenAI chat completions spec.
func stripStreamOptions(deps TransformDeps, payload map[string]interface{}, modelName string) {
	if _, ok := payload["stream_options"]; !ok {
		return
	}
	if modelSupportsCapability(deps, modelName, discovery.CapStreamOptions) {
		return
	}
	delete(payload, "stream_options")
	deps.Logger.Debug("stripped stream_options", "model", modelName)
}

func stripToolChoice(deps TransformDeps, payload map[string]interface{}, modelName string) {
	toolChoice, ok := payload["tool_choice"]
	if !ok {
		return
	}
	tc, ok := toolChoice.(string)
	if !ok || tc != "auto" {
		return
	}
	if modelSupportsCapability(deps, modelName, discovery.CapAutoToolChoice) {
		return
	}
	delete(payload, "tool_choice")
	deps.Logger.Debug("stripped tool_choice \"auto\"", "model", modelName)
}

func flattenContentArrays(deps TransformDeps, payload map[string]interface{}, modelName string) {
	messagesRaw, ok := payload["messages"]
	if !ok {
		return
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok {
		return
	}
	if modelSupportsCapability(deps, modelName, discovery.CapContentArrays) {
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
		deps.Logger.Debug("flattened content arrays", "model", modelName)
	}
}

func shouldStripReasoning(deps TransformDeps, modelName string) bool {
	return !modelSupportsCapability(deps, modelName, discovery.CapReasoning)
}

func processReasoningContent(deps TransformDeps, payload map[string]interface{}, modelName string) {
	if !shouldStripReasoning(deps, modelName) {
		return
	}
	if pipeline.StripReasoningContent(payload) {
		deps.Logger.Debug("stripped reasoning_content", "model", modelName)
	}
}

func processMessages(deps TransformDeps, payload map[string]interface{}, modelName string) {
	messagesRaw, ok := payload["messages"]
	if !ok {
		return
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok {
		return
	}
	flattenContentArrays(deps, payload, modelName)
	processReasoningContent(deps, payload, modelName)
	repaired, repairedMessages := repairMessageOrdering(messages)
	if !repaired {
		return
	}
	payload["messages"] = repairedMessages
	deps.Logger.Info("repaired invalid message role ordering", "model", modelName)
}

func applyDeepSeekFixes(deps TransformDeps, payload map[string]interface{}, modelName string) {
	if !strings.HasPrefix(modelName, "deepseek") {
		return
	}
	ensureDeepSeekReasoningContent(payload, deps.Logger)
	stripDeepSeekThinkingParams(payload, modelName, deps.Logger)
}

func SanitizePayload(deps TransformDeps, payload map[string]interface{}, modelName string) {
	stripStreamOptions(deps, payload, modelName)
	stripToolChoice(deps, payload, modelName)
	processMessages(deps, payload, modelName)
	applyDeepSeekFixes(deps, payload, modelName)
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

func stripDeepSeekThinkingParams(payload map[string]interface{}, modelName string, logger *slog.Logger) {
	if !strings.HasPrefix(modelName, "deepseek") {
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
			logger.Debug("stripped param ignored in thinking mode", "param", key, "model", modelName)
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
	if len(messages) >= math.MaxInt-4 {
		return false, messages
	}
	out := make([]interface{}, 0, len(messages)+4)

	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			out = append(out, msgRaw)
			continue
		}

		role, ok := msg["role"].(string)
		if !ok {
			out = append(out, msgRaw)
			continue
		}

		switch {
		case role == "tool":
			sawToolSinceLastAssistant = true
			out = append(out, msgRaw)

		case role == "user" && sawToolSinceLastAssistant:
			out = append(out, map[string]interface{}{"role": "assistant", "content": ""})
			out = append(out, msgRaw)
			repaired = true
			sawToolSinceLastAssistant = false

		default:
			sawToolSinceLastAssistant = false
			out = append(out, msgRaw)
		}
	}

	return repaired, out
}
