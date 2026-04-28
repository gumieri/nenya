package discovery

import (
	"log/slog"
	"strings"
)

type capabilityRule struct {
	prefix string
	caps   []string
}

var capabilityRules = []capabilityRule{
	{prefix: "claude-3", caps: []string{"vision", "tool_calls"}},
	{prefix: "claude-4", caps: []string{"vision", "tool_calls", "reasoning"}},
	{prefix: "gemini-2", caps: []string{"vision", "tool_calls", "reasoning"}},
	{prefix: "gemini-1.5", caps: []string{"vision", "tool_calls"}},
	{prefix: "gemini-1", caps: []string{"vision"}},
	{prefix: "gpt-4o", caps: []string{"vision", "tool_calls", "reasoning"}},
	{prefix: "gpt-4-turbo", caps: []string{"vision", "tool_calls"}},
	{prefix: "gpt-4", caps: []string{"tool_calls"}},
	{prefix: "o1", caps: []string{"reasoning", "tool_calls"}},
	{prefix: "o3", caps: []string{"reasoning", "tool_calls"}},
	{prefix: "o4", caps: []string{"reasoning", "tool_calls"}},
	{prefix: "deepseek-v4", caps: []string{"reasoning", "tool_calls"}},
	{prefix: "deepseek-r1", caps: []string{"reasoning", "tool_calls"}},
	{prefix: "glm-4", caps: []string{"tool_calls"}},
	{prefix: "glm-5", caps: []string{"tool_calls", "reasoning", "vision"}},
	{prefix: "qwen2.5", caps: []string{"tool_calls"}},
	{prefix: "qwen3", caps: []string{"tool_calls", "reasoning"}},
	{prefix: "mistral-large", caps: []string{"tool_calls", "reasoning"}},
	{prefix: "codestral", caps: []string{"tool_calls"}},
	{prefix: "devstral", caps: []string{"tool_calls"}},
	{prefix: "phi-4", caps: []string{"tool_calls", "reasoning"}},
	{prefix: "llama-4", caps: []string{"tool_calls", "reasoning"}},
}

func InferCapabilities(modelID string) *ModelMetadata {
	var meta ModelMetadata
	matched := false
	for _, rule := range capabilityRules {
		for _, seg := range ModelSegments(modelID) {
			if strings.HasPrefix(seg, rule.prefix) {
				slog.Debug("capability rule matched by segment",
					"model_id", modelID,
					"segment", seg,
					"rule_prefix", rule.prefix,
					"capabilities", rule.caps,
				)
				applyCapabilities(&meta, rule.caps)
				matched = true
				break
			}
		}
		if matched {
			break
		}
	}
	if !matched {
		slog.Debug("no capability rule matched",
			"model_id", modelID,
			"segments", ModelSegments(modelID),
		)
		return nil
	}
	slog.Info("model capabilities inferred",
		"model_id", modelID,
		"tool_calls", meta.SupportsToolCalls,
		"reasoning", meta.SupportsReasoning,
		"vision", meta.SupportsVision,
	)
	return &meta
}
