package config_test

import (
	"math"
	"testing"

	"github.com/nenya/config"
)

// TestModelRegistry_DeepSeekV41Catalog pins the DeepSeek V4.1-Flash catalog
// facts. Both the pricing on the legacy deepseek-v4.1-flash entry and the
// canonical deepseek-flash model ID (introduced with the 2026-09-10 release)
// must carry the V4.1-Flash rate card. A regression here is silent: a missing
// PricingOverride degrades /statsz cost totals without any runtime error, so
// the guard is explicit rather than inferred.
func TestModelRegistry_DeepSeekV41Catalog(t *testing.T) {
	const eps = 1e-9

	cases := []struct {
		model      string
		wantInput  float64
		wantOutput float64
	}{
		{model: "deepseek-v4.1-flash", wantInput: 0.30, wantOutput: 1.20},
		{model: "deepseek-flash", wantInput: 0.30, wantOutput: 1.20},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			entry, ok := config.ModelRegistry[tc.model]
			if !ok {
				t.Fatalf("model %q missing from ModelRegistry", tc.model)
			}
			if entry.Provider != "deepseek" {
				t.Errorf("provider = %q, want deepseek", entry.Provider)
			}
			if entry.MaxContext != 1_000_000 {
				t.Errorf("MaxContext = %d, want 1000000", entry.MaxContext)
			}
			if entry.MaxOutput != 384_000 {
				t.Errorf("MaxOutput = %d, want 384000", entry.MaxOutput)
			}
			if got := entry.Pricing.InputCostPer1M; math.Abs(got-tc.wantInput) > eps {
				t.Errorf("InputCostPer1M = %v, want %v", got, tc.wantInput)
			}
			if got := entry.Pricing.OutputCostPer1M; math.Abs(got-tc.wantOutput) > eps {
				t.Errorf("OutputCostPer1M = %v, want %v", got, tc.wantOutput)
			}
		})
	}
}
