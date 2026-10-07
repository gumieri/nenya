package gateway

import (
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
)

// TestTokenCalibrationDefaultsDrift pins config's mirrored defaults to
// infra's (NENYA-135): the two constant sets must not drift apart, since
// config resolves knobs before the tracker is built.
func TestTokenCalibrationDefaultsDrift(t *testing.T) {
	if config.DefaultTokenCalibrationMinObservations != infra.CalibrationDefaultMinObservations {
		t.Fatalf("min_observations default drifted: config %d vs infra %d",
			config.DefaultTokenCalibrationMinObservations, infra.CalibrationDefaultMinObservations)
	}
	if config.DefaultTokenCalibrationDecay != infra.CalibrationDefaultDecay {
		t.Fatalf("decay default drifted: config %v vs infra %v",
			config.DefaultTokenCalibrationDecay, infra.CalibrationDefaultDecay)
	}
	if config.DefaultTokenCalibrationClampMin != infra.CalibrationDefaultClampMin {
		t.Fatalf("clamp_min default drifted: config %v vs infra %v",
			config.DefaultTokenCalibrationClampMin, infra.CalibrationDefaultClampMin)
	}
	if config.DefaultTokenCalibrationClampMax != infra.CalibrationDefaultClampMax {
		t.Fatalf("clamp_max default drifted: config %v vs infra %v",
			config.DefaultTokenCalibrationClampMax, infra.CalibrationDefaultClampMax)
	}
}

// TestNewCalibrationParamsDisabled pins the disabled → nil contract used by
// both New and the SIGHUP reload path.
func TestNewCalibrationParamsDisabled(t *testing.T) {
	disabled := config.PtrTo(false)
	cfg := config.Config{Governance: config.GovernanceConfig{
		TokenCalibration: &config.TokenCalibrationConfig{Enabled: disabled},
	}}
	if params := newCalibrationParams(&cfg); params != nil {
		t.Fatalf("disabled loop must resolve to nil params, got %+v", params)
	}

	cfg.Governance.TokenCalibration.Enabled = config.PtrTo(true)
	cfg.Governance.TokenCalibration.MinObservations = 25
	params := newCalibrationParams(&cfg)
	if params == nil || !params.Enabled || params.MinObservations != 25 {
		t.Fatalf("enabled loop must resolve to non-nil params honoring overrides, got %+v", params)
	}
}
