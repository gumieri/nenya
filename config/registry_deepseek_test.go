package config_test

import (
	"math"
	"testing"
	"time"

	"github.com/nenya/config"
)

// TestModelRegistry_DeepSeekV41Catalog pins the DeepSeek V4.1-Flash catalog
// facts against the official rate card (verified 2026-09): standard (off-peak)
// $0.15/$0.60, peak exactly 2x ($0.30/$1.20), cache-hit input $0.003. The
// canonical deepseek-flash ID and the legacy deepseek-v4.1-flash /
// deepseek-v4-flash aliases must all carry it. A regression here is silent: a
// missing or wrong PricingOverride degrades /statsz cost totals and the
// max_cost_per_request guard without any runtime error, so the assertions are
// explicit rather than inferred.
func TestModelRegistry_DeepSeekV41Catalog(t *testing.T) {
	const eps = 1e-9

	cases := []struct {
		model       string
		wantInput   float64
		wantOutput  float64
		wantPeakIn  float64
		wantPeakOut float64
		wantCached  float64
	}{
		{model: "deepseek-v4.1-flash", wantInput: 0.15, wantOutput: 0.60, wantPeakIn: 0.30, wantPeakOut: 1.20, wantCached: 0.003},
		{model: "deepseek-flash", wantInput: 0.15, wantOutput: 0.60, wantPeakIn: 0.30, wantPeakOut: 1.20, wantCached: 0.003},
		{model: "deepseek-v4-flash", wantInput: 0.15, wantOutput: 0.60, wantPeakIn: 0.30, wantPeakOut: 1.20, wantCached: 0.003},
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
			checks := []struct {
				name string
				got  float64
				want float64
			}{
				{"InputCostPer1M", entry.Pricing.InputCostPer1M, tc.wantInput},
				{"OutputCostPer1M", entry.Pricing.OutputCostPer1M, tc.wantOutput},
				{"PeakInputCostPer1M", entry.Pricing.PeakInputCostPer1M, tc.wantPeakIn},
				{"PeakOutputCostPer1M", entry.Pricing.PeakOutputCostPer1M, tc.wantPeakOut},
				{"CachedInputCostPer1M", entry.Pricing.CachedInputCostPer1M, tc.wantCached},
			}
			for _, c := range checks {
				if math.Abs(c.got-c.want) > eps {
					t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
				}
			}
		})
	}
}

// TestModelRegistry_DeepSeekV4Pro pins the V4-Pro rate card: off-peak
// $0.66/$1.98, peak 2x ($1.32/$3.96), cache-hit $0.022. Since 2026-09-14
// DeepSeek serves deepseek-v4-pro with V4.1-Flash at Flash rates until
// V4.1-Pro launches, so this rate card overstates current spend — kept until
// the reroute is reversed or made permanent.
func TestModelRegistry_DeepSeekV4Pro(t *testing.T) {
	const eps = 1e-9
	entry, ok := config.ModelRegistry["deepseek-v4-pro"]
	if !ok {
		t.Fatal("deepseek-v4-pro missing from ModelRegistry")
	}
	checks := []struct {
		name string
		got  float64
		want float64
	}{
		{"InputCostPer1M", entry.Pricing.InputCostPer1M, 0.66},
		{"OutputCostPer1M", entry.Pricing.OutputCostPer1M, 1.98},
		{"PeakInputCostPer1M", entry.Pricing.PeakInputCostPer1M, 1.32},
		{"PeakOutputCostPer1M", entry.Pricing.PeakOutputCostPer1M, 3.96},
		{"CachedInputCostPer1M", entry.Pricing.CachedInputCostPer1M, 0.022},
	}
	for _, c := range checks {
		if math.Abs(c.got-c.want) > eps {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestBuiltinDeepSeekPeakWindows pins the verified DeepSeek peak schedule:
// 01:00–04:00 and 06:00–10:00 UTC Monday–Friday (everything else, weekends
// included, is off-peak).
func TestBuiltinDeepSeekPeakWindows(t *testing.T) {
	entry, ok := config.ProviderRegistry["deepseek"]
	if !ok {
		t.Fatal("deepseek missing from ProviderRegistry")
	}
	p := entry.ToProviderConfig()
	if len(p.PeakWindows) != 2 {
		t.Fatalf("peak windows = %d, want 2", len(p.PeakWindows))
	}
	utc := time.UTC
	cases := []struct {
		day  time.Weekday
		hh   int
		want bool
	}{
		// Wednesday inside the morning block.
		{time.Wednesday, 2, true},
		// Wednesday inside the afternoon block.
		{time.Wednesday, 8, true},
		// Wednesday in the 04:00–06:00 gap.
		{time.Wednesday, 5, false},
		// Wednesday off-peak evening.
		{time.Wednesday, 20, false},
		// Saturday inside what would be a peak block — weekends off-peak.
		{time.Saturday, 2, false},
		{time.Saturday, 8, false},
		// Sunday off-peak.
		{time.Sunday, 8, false},
	}
	// Anchor on a known week: 2026-10-04 is a Sunday.
	base := time.Date(2026, 10, 4, 0, 0, 0, 0, utc)
	for _, tc := range cases {
		t.Run(tc.day.String(), func(t *testing.T) {
			offset := (int(tc.day) - int(base.Weekday()) + 7) % 7
			at := base.AddDate(0, 0, offset).Add(time.Duration(tc.hh) * time.Hour)
			if got := p.IsPeakAt(at); got != tc.want {
				t.Errorf("IsPeakAt(%s %02d:00 UTC) = %v, want %v", tc.day, tc.hh, got, tc.want)
			}
		})
	}
}
