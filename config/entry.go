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

// ModelEntry defines a model in the static ModelRegistry: its provider,
// context limits, wire format, capabilities, scoring bonus, and pricing.
type ModelEntry struct {
	Provider     string
	MaxContext   int
	MaxOutput    int
	Format       string          `json:"format,omitempty"`
	ScoreBonus   float64         `json:"score_bonus,omitempty"`
	Capabilities []string        `json:"capabilities,omitempty"`
	Pricing      PricingOverride `json:"pricing,omitempty"`
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
