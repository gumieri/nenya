package discovery

import (
	"nenya/internal/config"
)

type ModelMetadata struct {
	SupportsStreamOptions  bool   `json:"supports_stream_options,omitempty"`
	SupportsAutoToolChoice bool   `json:"supports_auto_tool_choice,omitempty"`
	SupportsContentArrays  bool   `json:"supports_content_arrays,omitempty"`
	SupportsToolCalls      bool   `json:"supports_tool_calls,omitempty"`
	SupportsReasoning      bool   `json:"supports_reasoning,omitempty"`
	SupportsVision         bool   `json:"supports_vision,omitempty"`

	ScoreBonus float64 `json:"score_bonus,omitempty"`

	Pricing    *config.PricingOverride `json:"pricing,omitempty"`
	ParsedFrom map[string]interface{} `json:"parsed_from,omitempty"`
}

func applyCapabilities(meta *ModelMetadata, caps []string) *ModelMetadata {
	if meta == nil || len(caps) == 0 {
		return meta
	}
	for _, c := range caps {
		switch c {
		case "vision":
			meta.SupportsVision = true
		case "tool_calls":
			meta.SupportsToolCalls = true
		case "reasoning":
			meta.SupportsReasoning = true
		case "content_arrays":
			meta.SupportsContentArrays = true
		case "stream_options":
			meta.SupportsStreamOptions = true
		case "auto_tool_choice":
			meta.SupportsAutoToolChoice = true
		}
	}
	return meta
}