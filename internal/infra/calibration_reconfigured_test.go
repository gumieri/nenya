package infra

import "testing"

// TestCalibrationTracker_Reconfigured verifies the SIGHUP reload path:
// learned sums carry over under the new params (NENYA-135), and nil-safe
// reconfiguration yields nil.
func TestCalibrationTracker_Reconfigured(t *testing.T) {
	tr := NewCalibrationTracker(CalibrationParams{Enabled: true, MinObservations: 10, Decay: 0.99, ClampMin: 0.5, ClampMax: 4.0})
	for i := 0; i < 100; i++ {
		tr.RecordEstimate("m", 100)
		tr.RecordActual("m", 200)
	}
	before := tr.Snapshot()["m"]

	// Knob change (stricter clamp): sums carry, clamp applies.
	next := tr.Reconfigured(CalibrationParams{Enabled: true, MinObservations: 10, Decay: 0.99, ClampMin: 1.9, ClampMax: 1.9})
	if next == tr {
		t.Fatal("Reconfigured must return a new tracker")
	}
	after := next.Snapshot()["m"]
	if after.EstimateTokens != before.EstimateTokens || after.ActualTokens != before.ActualTokens {
		t.Fatalf("learned sums lost: %v -> %v", before, after)
	}
	if got := next.Ratio("m"); got != 1.9 {
		t.Fatalf("new clamp not applied: ratio = %v, want 1.9", got)
	}
	// The old tracker is untouched (no aliasing).
	if got := tr.Ratio("m"); got < 1.99 || got > 2.01 {
		t.Fatalf("old tracker mutated by Reconfigured: %v", got)
	}

	// Independent mutation after the split.
	next.RecordActual("m", 1000000)
	if got := tr.Ratio("m"); got > 2.01 {
		t.Fatalf("deep copy failed: old tracker ratio polluted to %v", got)
	}

	// Nil-safe.
	var nilTr *CalibrationTracker
	if nilTr.Reconfigured(CalibrationParams{Enabled: true}) != nil {
		t.Fatal("nil Reconfigured must yield nil")
	}
}
