package config

import (
	"strings"
	"testing"
)

// The warm-cache tie-break knobs (NENYA-202 follow-up): the near-tie band
// width and the token floor both default (5% / 8192), accept explicit
// values, and treat explicit 0 band as "never a near-tie" / explicit 0 floor
// as "no floor".
func TestEffectiveWarmCacheBandPct(t *testing.T) {
	twenty := 20.0
	zero := 0.0
	tests := []struct {
		name string
		set  *float64
		want float64
	}{
		{"nil applies the 5% default", nil, 5.0},
		{"explicit value wins", &twenty, 20.0},
		{"explicit zero disables the band", &zero, 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := GovernanceConfig{AutoReorderWarmCacheBandPct: tt.set}
			if got := g.EffectiveWarmCacheBandPct(); got != tt.want {
				t.Fatalf("EffectiveWarmCacheBandPct() = %v, want %v", got, tt.want)
			}
		})
	}
	// The nil-receiver path must not panic (nil-safe helpers).
	var g *GovernanceConfig
	if got := g.EffectiveWarmCacheBandPct(); got != 5.0 {
		t.Fatalf("nil receiver must default: got %v", got)
	}
}

func TestEffectiveWarmCacheMinTokens(t *testing.T) {
	fortyK := 4096
	zero := 0
	tests := []struct {
		name string
		set  *int
		want int
	}{
		{"nil applies the 8192 default", nil, 8192},
		{"explicit value wins", &fortyK, 4096},
		{"explicit zero disables the floor", &zero, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := GovernanceConfig{AutoReorderWarmCacheMinTokens: tt.set}
			if got := g.EffectiveWarmCacheMinTokens(); got != tt.want {
				t.Fatalf("EffectiveWarmCacheMinTokens() = %v, want %v", got, tt.want)
			}
		})
	}
	var g *GovernanceConfig
	if got := g.EffectiveWarmCacheMinTokens(); got != 8192 {
		t.Fatalf("nil receiver must default: got %v", got)
	}
}

// Validation: a negative band, a band above 100, and a negative floor are
// all rejected at load.
func TestWarmCacheKnobsValidation(t *testing.T) {
	negBand := -1.0
	bigBand := 150.0
	negFloor := -5

	tests := []struct {
		name string
		cfg  *Config
		want string
	}{
		{
			"negative band rejected",
			&Config{Governance: GovernanceConfig{AutoReorderWarmCacheBandPct: &negBand}},
			"auto_reorder_warm_cache_band_pct",
		},
		{
			"band above 100 rejected",
			&Config{Governance: GovernanceConfig{AutoReorderWarmCacheBandPct: &bigBand}},
			"auto_reorder_warm_cache_band_pct",
		},
		{
			"negative floor rejected",
			&Config{Governance: GovernanceConfig{AutoReorderWarmCacheMinTokens: &negFloor}},
			"auto_reorder_warm_cache_min_tokens",
		},
		{
			"in-range values pass",
			&Config{Governance: GovernanceConfig{}},
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateWarmCacheKnobs(tt.cfg)
			if tt.want == "" {
				if len(errs) != 0 {
					t.Fatalf("expected no errors, got %v", errs)
				}
				return
			}
			found := false
			for _, e := range errs {
				if strings.Contains(e, tt.want) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected an error naming %s, got %v", tt.want, errs)
			}
		})
	}
}
