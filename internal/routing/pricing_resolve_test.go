package routing

import (
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/discovery"
)

// TestResolveModelPricing_StaticFallbackPreservesPeak ensures the static
// registry fallback carries the peak/cached dimensions (not just the flat
// baseline), so the entry does not silently lose them when a consumer is added.
func TestResolveModelPricing_StaticFallbackPreservesPeak(t *testing.T) {
	const id = "cost-model-test-static-peak"
	config.ModelRegistry[id] = config.ModelEntry{
		Provider: "deepseek",
		Pricing: config.PricingOverride{
			InputCostPer1M:       0.15,
			OutputCostPer1M:      0.60,
			PeakInputCostPer1M:   0.30,
			PeakOutputCostPer1M:  1.20,
			CachedInputCostPer1M: 0.05,
		},
	}
	t.Cleanup(func() { delete(config.ModelRegistry, id) })

	p := resolveModelPricing(config.AgentModel{Model: id}, nil)
	if p == nil {
		t.Fatal("expected pricing entry")
	}
	if !p.HasPeak() || p.PeakInputCostPer1M != 0.30 || p.PeakOutputCostPer1M != 1.20 {
		t.Errorf("peak fields lost: %+v", p)
	}
	if p.CachedInputCostPer1M != 0.05 {
		t.Errorf("cached input lost: %+v", p)
	}
	if !p.HasStandardRate() {
		t.Errorf("standard rate lost: %+v", p)
	}
}

// TestResolveAgentPricing_AveragesBaselineAndPeak pins the Phase 006 contract:
// the routing/display average carries the standard baseline AND the peak/cached
// dimensions, so routing stays time-invariant while display can show both.
func TestResolveAgentPricing_AveragesBaselineAndPeak(t *testing.T) {
	const id = "cost-model-test-agent-peak"
	config.ModelRegistry[id] = config.ModelEntry{
		Provider: "deepseek",
		Pricing: config.PricingOverride{
			InputCostPer1M:       0.15,
			OutputCostPer1M:      0.60,
			PeakInputCostPer1M:   0.30,
			PeakOutputCostPer1M:  1.20,
			CachedInputCostPer1M: 0.05,
		},
	}
	t.Cleanup(func() { delete(config.ModelRegistry, id) })

	agent := config.AgentConfig{}
	agent.Models = []config.AgentModel{{Model: id}, {Model: id}}
	agents := map[string]config.AgentConfig{"cost-agent": agent}

	pricing := ResolveAgentPricing("cost-agent", agents, nil)
	if !pricing.HasPricing {
		t.Fatal("expected pricing")
	}
	if pricing.InputCostPer1M != 0.15 || pricing.OutputCostPer1M != 0.60 {
		t.Errorf("baseline average = %+v", pricing)
	}
	if pricing.PeakInputCostPer1M != 0.30 || pricing.PeakOutputCostPer1M != 1.20 {
		t.Errorf("peak average = %+v", pricing)
	}
	if pricing.CachedInputCostPer1M != 0.05 {
		t.Errorf("cached average = %+v", pricing)
	}
}

// TestResolveAgentPricing_MixedChain pins the denominator rule: every
// dimension averages over the models that declare a standard rate, and a
// peak-less priced model contributes 0 to the peak averages.
func TestResolveAgentPricing_MixedChain(t *testing.T) {
	const flatID = "cost-model-test-flat"
	const peakID = "cost-model-test-peak-mixed"
	config.ModelRegistry[flatID] = config.ModelEntry{
		Provider: "p",
		Pricing:  config.PricingOverride{InputCostPer1M: 0.10, OutputCostPer1M: 0.40},
	}
	config.ModelRegistry[peakID] = config.ModelEntry{
		Provider: "p",
		Pricing: config.PricingOverride{
			InputCostPer1M:      0.15,
			OutputCostPer1M:     0.60,
			PeakInputCostPer1M:  0.30,
			PeakOutputCostPer1M: 1.20,
		},
	}
	t.Cleanup(func() {
		delete(config.ModelRegistry, flatID)
		delete(config.ModelRegistry, peakID)
	})

	agent := config.AgentConfig{}
	agent.Models = []config.AgentModel{{Model: flatID}, {Model: peakID}}
	pricing := ResolveAgentPricing("a", map[string]config.AgentConfig{"a": agent}, nil)
	if !pricing.HasPricing {
		t.Fatal("expected pricing")
	}
	// Baseline average over both models.
	if pricing.InputCostPer1M != (0.10+0.15)/2 || pricing.OutputCostPer1M != (0.40+0.60)/2 {
		t.Errorf("baseline average = %+v", pricing)
	}
	// Peak average shares the denominator: flat model contributes 0.
	if pricing.PeakInputCostPer1M != 0.30/2 || pricing.PeakOutputCostPer1M != 1.20/2 {
		t.Errorf("peak average = %+v", pricing)
	}
	if pricing.CachedInputCostPer1M != 0 {
		t.Errorf("cached average = %+v, want 0", pricing)
	}
}

// TestResolveModelPricing_CatalogOverlayPreservesStaticPeak pins the Phase 006
// overlay: a discovered (baseline-only) catalog entry must not shadow the
// static registry's peak/cached dimensions.
func TestResolveModelPricing_CatalogOverlayPreservesStaticPeak(t *testing.T) {
	const id = "cost-model-test-overlay"
	config.ModelRegistry[id] = config.ModelEntry{
		Provider: "p",
		Pricing: config.PricingOverride{
			InputCostPer1M:       0.15,
			OutputCostPer1M:      0.60,
			PeakInputCostPer1M:   0.30,
			PeakOutputCostPer1M:  1.20,
			CachedInputCostPer1M: 0.05,
		},
	}
	t.Cleanup(func() { delete(config.ModelRegistry, id) })

	catalog := discovery.NewModelCatalog()
	catalog.Add(discovery.DiscoveredModel{ID: id, Provider: "p", Pricing: &discovery.PricingEntry{
		InputCostPer1M: 0.99, OutputCostPer1M: 1.99, Currency: "USD",
	}})

	p := resolveModelPricing(config.AgentModel{Model: id}, catalog)
	if p == nil {
		t.Fatal("expected pricing")
	}
	if p.InputCostPer1M != 0.99 || p.OutputCostPer1M != 1.99 {
		t.Errorf("discovered baseline must win: %+v", p)
	}
	if p.PeakInputCostPer1M != 0.30 || p.PeakOutputCostPer1M != 1.20 {
		t.Errorf("static peak shadowed by discovered entry: %+v", p)
	}
	if p.CachedInputCostPer1M != 0.05 {
		t.Errorf("static cached shadowed by discovered entry: %+v", p)
	}
}
