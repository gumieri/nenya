package routing

import (
	"testing"

	"github.com/nenya/config"
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
