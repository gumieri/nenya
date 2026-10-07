package gateway

import (
	"math"
	"testing"

	"github.com/nenya/internal/infra"
)

// TestCalibratedCountTokens verifies the NENYA-135 application point: raw
// cl100k below the warm-up floor, drift-scaled above it, and nil-tracker
// passthrough.
func TestCalibratedCountTokens(t *testing.T) {
	text := "hello brave new world of token estimation" // > 0 cl100k tokens
	g := &NenyaGateway{Logger: testLogger()}

	if raw := g.CountTokens(text); raw == 0 {
		t.Fatal("test requires a non-zero raw estimate")
	}

	t.Run("nil tracker = raw cl100k", func(t *testing.T) {
		if got := g.CalibratedCountTokens(text, "some-model"); got != g.CountTokens(text) {
			t.Fatalf("nil tracker must pass through raw estimate: %d vs %d", got, g.CountTokens(text))
		}
	})

	t.Run("warm-up and drift", func(t *testing.T) {
		g.Calibration = infra.NewCalibrationTracker(infra.CalibrationParams{
			Enabled:         true,
			MinObservations: 50,
			Decay:           0.99,
			ClampMin:        0.5,
			ClampMax:        4.0,
		})
		// Warm-up: estimates without ground-truth actuals keep ratio 1.0.
		g.Calibration.RecordEstimate("gemini-3-pro", 500)
		if got := g.CalibratedCountTokens(text, "gemini-3-pro"); got != g.CountTokens(text) {
			t.Fatalf("warm-up must return the raw estimate: %d vs %d", got, g.CountTokens(text))
		}
		// Drift: actuals consistently 2x estimates for this model only.
		for i := 0; i < 200; i++ {
			g.Calibration.RecordEstimate("gemini-3-pro", 100)
			g.Calibration.RecordActual("gemini-3-pro", 200)
		}
		raw := g.CountTokens(text)
		got := g.CalibratedCountTokens(text, "gemini-3-pro")
		want := math.Round(float64(raw) * g.Calibration.Ratio("gemini-3-pro"))
		if got != int(want) {
			t.Fatalf("calibrated = %d, want %d (raw %d, ratio %v)", got, int(want), raw, g.Calibration.Ratio("gemini-3-pro"))
		}
		if got <= raw {
			t.Fatalf("2x drift must expand the estimate: %d vs raw %d", got, raw)
		}
		// Other models are untouched.
		if other := g.CalibratedCountTokens(text, "gpt-5"); other != raw {
			t.Fatalf("unobserved model must stay raw: %d vs %d", other, raw)
		}
	})
}
