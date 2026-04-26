package config

import (
	"errors"
	"fmt"
)

// PricingOverride represents pricing data for static model registry entries.
// It is converted to discovery.PricingEntry (which adds Currency field and CalculateCost method)
// when merging static entries into the ModelCatalog. The conversion is implicit via field
// name matching in discovery.MergeCatalog.
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
	if err := e.Pricing.Validate(); err != nil {
		return err
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
