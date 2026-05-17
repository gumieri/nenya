package discovery

import (
	"log/slog"
	"strings"
)

type Capability string

const (
	CapVision         Capability = "vision"
	CapToolCalls      Capability = "tool_calls"
	CapReasoning      Capability = "reasoning"
	CapContentArrays  Capability = "content_arrays"
	CapStreamOptions  Capability = "stream_options"
	CapAutoToolChoice Capability = "auto_tool_choice"
)

type capabilityRule struct {
	prefix string
	caps   []Capability
}

var capabilityRules = []capabilityRule{
	{prefix: "claude-3", caps: []Capability{CapVision, CapToolCalls, CapContentArrays, CapAutoToolChoice}},
	{prefix: "claude-4", caps: []Capability{CapVision, CapToolCalls, CapReasoning, CapContentArrays, CapAutoToolChoice}},
	{prefix: "gemini-2", caps: []Capability{CapVision, CapToolCalls, CapReasoning, CapContentArrays, CapAutoToolChoice}},
	{prefix: "gemini-1.5", caps: []Capability{CapVision, CapToolCalls, CapContentArrays, CapAutoToolChoice}},
	{prefix: "gemini-1", caps: []Capability{CapVision}},
	{prefix: "gpt-4o", caps: []Capability{CapVision, CapToolCalls, CapReasoning, CapContentArrays, CapAutoToolChoice, CapStreamOptions}},
	{prefix: "gpt-4-turbo", caps: []Capability{CapVision, CapToolCalls, CapContentArrays, CapAutoToolChoice, CapStreamOptions}},
	{prefix: "gpt-4", caps: []Capability{CapToolCalls, CapContentArrays, CapAutoToolChoice, CapStreamOptions}},
	{prefix: "o1", caps: []Capability{CapReasoning, CapToolCalls, CapContentArrays, CapAutoToolChoice, CapStreamOptions}},
	{prefix: "o3", caps: []Capability{CapReasoning, CapToolCalls, CapContentArrays, CapAutoToolChoice, CapStreamOptions}},
	{prefix: "o4", caps: []Capability{CapReasoning, CapToolCalls, CapContentArrays, CapAutoToolChoice, CapStreamOptions}},
	{prefix: "deepseek-v4", caps: []Capability{CapReasoning, CapToolCalls, CapContentArrays, CapAutoToolChoice, CapStreamOptions}},
	{prefix: "deepseek-r1", caps: []Capability{CapReasoning, CapToolCalls, CapContentArrays, CapAutoToolChoice}},
	{prefix: "glm-4", caps: []Capability{CapToolCalls, CapContentArrays, CapAutoToolChoice, CapStreamOptions}},
	{prefix: "glm-5", caps: []Capability{CapToolCalls, CapReasoning, CapVision, CapContentArrays, CapAutoToolChoice, CapStreamOptions}},
	{prefix: "qwen2.5", caps: []Capability{CapToolCalls, CapContentArrays, CapAutoToolChoice, CapStreamOptions}},
	{prefix: "qwen3", caps: []Capability{CapToolCalls, CapReasoning, CapContentArrays, CapAutoToolChoice, CapStreamOptions}},
	{prefix: "mistral-large", caps: []Capability{CapToolCalls, CapReasoning, CapContentArrays, CapAutoToolChoice, CapStreamOptions}},
	{prefix: "codestral", caps: []Capability{CapToolCalls, CapContentArrays, CapAutoToolChoice, CapStreamOptions}},
	{prefix: "devstral", caps: []Capability{CapToolCalls, CapContentArrays, CapAutoToolChoice, CapStreamOptions}},
	{prefix: "phi-4", caps: []Capability{CapToolCalls, CapReasoning, CapContentArrays, CapAutoToolChoice, CapStreamOptions}},
	{prefix: "llama-4", caps: []Capability{CapToolCalls, CapReasoning, CapContentArrays, CapAutoToolChoice, CapStreamOptions}},
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
	slog.Debug("model capabilities inferred",
		"model_id", modelID,
		"tool_calls", meta.SupportsToolCalls,
		"reasoning", meta.SupportsReasoning,
		"vision", meta.SupportsVision,
	)
	return &meta
}

func InferFormat(modelID string) string {
	switch {
	case strings.HasPrefix(modelID, "claude-"):
		return "anthropic"
	case strings.HasPrefix(modelID, "gemini-"):
		return "gemini"
	default:
		return ""
	}
}
