package config

import (
	"errors"
	"fmt"
)

// PricingOverride allows overriding a model's default per-token pricing.
// Zero values mean "use the built-in pricing".
type PricingOverride struct {
	InputCostPer1M  float64 `json:"input_cost_per_1m"`
	OutputCostPer1M float64 `json:"output_cost_per_1m"`
}

func (p PricingOverride) IsZero() bool {
	return p.InputCostPer1M == 0 && p.OutputCostPer1M == 0
}

func (p PricingOverride) Validate() error {
	if p.InputCostPer1M < 0 {
		return fmt.Errorf("PricingOverride.InputCostPer1M must be non-negative, got %f", p.InputCostPer1M)
	}
	if p.OutputCostPer1M < 0 {
		return fmt.Errorf("PricingOverride.OutputCostPer1M must be non-negative, got %f", p.OutputCostPer1M)
	}
	return nil
}

// ModelThinkingConfig captures reasoning/thinking capabilities for supported models
// in the static ModelRegistry, describing what the model supports natively.
//
// Fields:
//   - Min: Minimum number of thinking tokens (optional, default 0)
//   - Max: Maximum number of thinking tokens (optional, default 0)
//   - ZeroAllowed: Whether zero thinking tokens are permitted (default false)
//   - DynamicAllowed: Whether dynamic thinking is supported (default false)
//   - Levels: Available thinking intensity levels like "low", "medium", "high" (optional)
//
// Zero values for Min/Max mean the field is unset (no thinking).
// Non-zero Min implies thinking is enabled with the specified budget.
// If both Min and Max are zero, ZeroAllowed must be true.
type ModelThinkingConfig struct {
	Min            int      `json:"min,omitempty"`
	Max            int      `json:"max,omitempty"`
	ZeroAllowed    bool     `json:"zero_allowed,omitempty"`
	DynamicAllowed bool     `json:"dynamic_allowed,omitempty"`
	Levels         []string `json:"levels,omitempty"`
}

// Validate checks that Min <= Max when both fields are set and rejects
// negative values for all fields.
//
// Returns an error if:
//   - Min or Max is negative
//   - Both Min and Max are set but Min > Max
func (c ModelThinkingConfig) Validate() error {
	if c.Min < 0 {
		return fmt.Errorf("ModelThinkingConfig.Min must be non-negative, got %d", c.Min)
	}
	if c.Max < 0 {
		return fmt.Errorf("ModelThinkingConfig.Max must be non-negative, got %d", c.Max)
	}
	if c.Min > 0 && c.Max > 0 && c.Min > c.Max {
		return fmt.Errorf("ModelThinkingConfig.Min (%d) must be <= Max (%d)", c.Min, c.Max)
	}
	return nil
}

// ModelEntry defines a model in the static ModelRegistry: its provider,
// context limits, wire format, capabilities, scoring bonus, and pricing.
type ModelEntry struct {
	Provider     string
	MaxContext   int
	MaxOutput    int
	Format       string              `json:"format,omitempty"`
	Thinking     ModelThinkingConfig `json:"thinking,omitempty"`
	ScoreBonus   float64             `json:"score_bonus,omitempty"`
	Capabilities []string            `json:"capabilities,omitempty"`
	Pricing      PricingOverride     `json:"pricing,omitempty"`
}

func (e ModelEntry) Validate() error {
	if e.Provider == "" {
		return errors.New("ModelEntry.Provider is required")
	}
	if e.MaxContext < 0 {
		return fmt.Errorf("ModelEntry.MaxContext must be non-negative, got %d", e.MaxContext)
	}
	if e.MaxOutput < 0 {
		return fmt.Errorf("ModelEntry.MaxOutput must be non-negative, got %d", e.MaxOutput)
	}
	if err := e.Thinking.Validate(); err != nil {
		return err
	}
	if err := e.Pricing.Validate(); err != nil {
		return err
	}
	return nil
}

// ModelRef is a lightweight reference to a model with its context limits.
type ModelRef struct {
	ID         string
	MaxContext int
	MaxOutput  int
}

// ProviderEntry defines a built-in provider's URL, auth style, API
// format, format-specific URL overrides, and associated model references.
type ProviderEntry struct {
	URL        string
	AuthStyle  string
	ApiFormat  string
	FormatURLs map[string]string `json:"format_urls,omitempty"`
	Models     []ModelRef
}

func (e ProviderEntry) ToProviderConfig() ProviderConfig {
	return ProviderConfig{
		URL:        e.URL,
		AuthStyle:  e.AuthStyle,
		ApiFormat:  e.ApiFormat,
		FormatURLs: e.FormatURLs,
	}
}
