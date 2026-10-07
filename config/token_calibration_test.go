package config

import (
	"testing"
)

// TestValidateTokenCalibration covers the NENYA-135 knob validation.
func TestValidateTokenCalibration(t *testing.T) {
	cases := []struct {
		name      string
		cfg       *TokenCalibrationConfig
		wantError bool
	}{
		{name: "nil config", cfg: nil},
		{name: "empty config (defaults)", cfg: &TokenCalibrationConfig{}},
		{name: "valid", cfg: &TokenCalibrationConfig{MinObservations: 100, Decay: 0.98, RatioClampMin: 0.25, RatioClampMax: 5.0}},
		{name: "negative min observations", cfg: &TokenCalibrationConfig{MinObservations: -1}, wantError: true},
		{name: "decay zero-exclusive low", cfg: &TokenCalibrationConfig{Decay: 0}, wantError: false}, // 0 = default
		{name: "decay at 1 rejected", cfg: &TokenCalibrationConfig{Decay: 1.0}, wantError: true},
		{name: "decay above 1 rejected", cfg: &TokenCalibrationConfig{Decay: 1.5}, wantError: true},
		{name: "negative clamp min", cfg: &TokenCalibrationConfig{RatioClampMin: -0.5}, wantError: true},
		{name: "clamp max below min", cfg: &TokenCalibrationConfig{RatioClampMin: 2.0, RatioClampMax: 1.0}, wantError: true},
		{name: "clamp max zero means default", cfg: &TokenCalibrationConfig{RatioClampMin: 2.0}, wantError: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateTokenCalibration(tc.cfg)
			if tc.wantError && len(errs) == 0 {
				t.Fatal("expected validation errors, got none")
			}
			if !tc.wantError && len(errs) != 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
		})
	}
}

// TestEffectiveTokenCalibrationParams pins the defaults and override paths.
func TestEffectiveTokenCalibrationParams(t *testing.T) {
	t.Run("nil config = enabled defaults", func(t *testing.T) {
		g := &GovernanceConfig{}
		enabled, minObs, decay, clampMin, clampMax := g.EffectiveTokenCalibrationParams()
		if !enabled || minObs != 50 || decay != 0.99 || clampMin != 0.5 || clampMax != 4.0 {
			t.Fatalf("got enabled=%v minObs=%d decay=%v clamp=[%v,%v]", enabled, minObs, decay, clampMin, clampMax)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		g := &GovernanceConfig{TokenCalibration: &TokenCalibrationConfig{Enabled: PtrTo(false)}}
		if enabled, _, _, _, _ := g.EffectiveTokenCalibrationParams(); enabled {
			t.Fatal("expected disabled")
		}
	})
	t.Run("clamp max floors at min when unset", func(t *testing.T) {
		g := &GovernanceConfig{TokenCalibration: &TokenCalibrationConfig{RatioClampMin: 5.0}}
		enabled, _, _, clampMin, clampMax := g.EffectiveTokenCalibrationParams()
		if !enabled || clampMin != 5.0 || clampMax != 5.0 {
			t.Fatalf("expected [5,5] (min above default max, max unset), got enabled=%v clamp=[%v,%v]", enabled, clampMin, clampMax)
		}
	})
	t.Run("overrides", func(t *testing.T) {
		g := &GovernanceConfig{TokenCalibration: &TokenCalibrationConfig{MinObservations: 200, Decay: 0.95, RatioClampMin: 0.25, RatioClampMax: 6.0}}
		enabled, minObs, decay, clampMin, clampMax := g.EffectiveTokenCalibrationParams()
		if !enabled || minObs != 200 || decay != 0.95 || clampMin != 0.25 || clampMax != 6.0 {
			t.Fatalf("overrides lost: enabled=%v minObs=%d decay=%v clamp=[%v,%v]", enabled, minObs, decay, clampMin, clampMax)
		}
	})
}
