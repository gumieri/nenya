package discovery

import (
	"fmt"
	"strings"

	"nenya/internal/config"
)

const (
	minScoreBonus = -1000.0
	maxScoreBonus = 1000.0
)

// ModelRanking represents quality metrics for a model from a specific
// ranking source (e.g., LMSYS leaderboard). AsOf is the date when
// the ranking was last updated (YYYY-MM-DD format).
type ModelRanking struct {
	Elo  *float64 `json:"elo,omitempty"`
	Rank *int     `json:"rank,omitempty"`
	AsOf string   `json:"as_of,omitempty"`
}

// ModelMetadata contains optional provider-agnostic information about
// a model, including capabilities, family classification, pricing overrides,
// and quality rankings from various sources.
type ModelMetadata struct {
	SupportsStreamOptions  bool                       `json:"supports_stream_options,omitempty"`
	SupportsAutoToolChoice bool                       `json:"supports_auto_tool_choice,omitempty"`
	SupportsContentArrays  bool                       `json:"supports_content_arrays,omitempty"`
	SupportsToolCalls      bool                       `json:"supports_tool_calls,omitempty"`
	SupportsReasoning      bool                       `json:"supports_reasoning,omitempty"`
	SupportsVision         bool                       `json:"supports_vision,omitempty"`

	ScoreBonus float64 `json:"score_bonus,omitempty"`

	Family    string                 `json:"family,omitempty"`
	Rankings  map[string]ModelRanking `json:"rankings,omitempty"`

	Pricing    *config.PricingOverride `json:"pricing,omitempty"`
	ParsedFrom map[string]interface{}  `json:"parsed_from,omitempty"`
}

// Validate returns an error if ModelMetadata contains invalid data.
func (m *ModelMetadata) Validate() error {
	if m.ScoreBonus < minScoreBonus || m.ScoreBonus > maxScoreBonus {
		return fmt.Errorf("scoreBonus out of range [%f, %f]: %f", minScoreBonus, maxScoreBonus, m.ScoreBonus)
	}
	if m.Family != "" && len(strings.TrimSpace(m.Family)) == 0 {
		return fmt.Errorf("family must be non-empty when set")
	}
	return nil
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
