package discovery

// ModelMetadata contains capability flags and dynamic properties discovered from providers.
// Supported capability flags are based on OpenAI Completions API features.
type ModelMetadata struct {
	// Standard OpenAI capabilities
	SupportsStreamOptions  bool `json:"supports_stream_options,omitempty"`
	SupportsAutoToolChoice bool `json:"supports_auto_tool_choice,omitempty"`
	SupportsContentArrays  bool `json:"supports_content_arrays,omitempty"`  // Vision, tool calls
	SupportsToolCalls      bool `json:"supports_tool_calls,omitempty"`
	SupportsReasoning      bool `json:"supports_reasoning,omitempty"`
	SupportsVision         bool `json:"supports_vision,omitempty"`

	// Configurable scoring bonus for routing (default 0, positive = prefer this model)
	ScoreBonus float64 `json:"score_bonus,omitempty"`

	// Provider-specific metadata (parsing hints, etc.)
	ParsedFrom map[string]interface{} `json:"parsed_from,omitempty"`
}