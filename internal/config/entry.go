package config

import (
	"errors"
	"fmt"
)

type PricingOverride struct {
	InputCostPer1M  float64 `json:"input_cost_per_1m"`
	OutputCostPer1M float64 `json:"output_cost_per_1m"`
}

func (p PricingOverride) IsZero() bool {
	return p.InputCostPer1M == 0 && p.OutputCostPer1M == 0
}

type ModelEntry struct {
	Provider     string
	MaxContext   int
	MaxOutput    int
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
	return nil
}

type ModelRef struct {
	ID         string
	MaxContext int
	MaxOutput  int
}

type ProviderEntry struct {
	URL           string
	RoutePrefixes []string
	AuthStyle     string
	ApiFormat     string
	Models        []ModelRef
}

func (e ProviderEntry) ToProviderConfig() ProviderConfig {
	return ProviderConfig{
		URL:           e.URL,
		RoutePrefixes: e.RoutePrefixes,
		AuthStyle:     e.AuthStyle,
		ApiFormat:     e.ApiFormat,
	}
}
