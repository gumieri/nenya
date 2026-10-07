package infra

import (
	"math"
	"sort"
	"testing"
)

// TestCalibrationTracker_ConvergesAndWarmup verifies the NENYA-135 loop's
// core behavior: ratio 1.0 during warm-up, convergence toward the true
// drift factor once the decaying estimate mass crosses MinObservations,
// and the clamp bounding pathological windows.
func TestCalibrationTracker_ConvergesAndWarmup(t *testing.T) {
	tr := NewCalibrationTracker(CalibrationParams{
		Enabled:         true,
		MinObservations: 100,
		Decay:           0.99,
		ClampMin:        0.5,
		ClampMax:        4.0,
	})

	// Warm-up: estimates recorded, no ratio applied yet.
	tr.RecordEstimate("gemini-3-pro", 200)
	if got := tr.Ratio("gemini-3-pro"); got != 1.0 {
		t.Fatalf("warm-up ratio = %v, want 1.0", got)
	}

	// Gemini-family drift: actual is consistently 2x the cl100k estimate.
	for i := 0; i < 500; i++ {
		tr.RecordEstimate("gemini-3-pro", 100)
		tr.RecordActual("gemini-3-pro", 200)
	}
	got := tr.Ratio("gemini-3-pro")
	if math.Abs(got-2.0) > 0.05 {
		t.Fatalf("ratio = %v, want ~2.0 for a consistent 2x drift", got)
	}

	// Unobserved models stay at 1.0.
	if got := tr.Ratio("gpt-5"); got != 1.0 {
		t.Fatalf("unobserved ratio = %v, want 1.0", got)
	}

	// A pathological window is clamped.
	for i := 0; i < 500; i++ {
		tr.RecordEstimate("deepseek-r2", 10)
		tr.RecordActual("deepseek-r2", 1000)
	}
	if got := tr.Ratio("deepseek-r2"); got != 4.0 {
		t.Fatalf("clamped ratio = %v, want 4.0", got)
	}

	// Zero/negative records are ignored (never poison the sums).
	before := tr.Snapshot()["gemini-3-pro"]
	tr.RecordEstimate("gemini-3-pro", 0)
	tr.RecordActual("gemini-3-pro", -5)
	if after := tr.Snapshot()["gemini-3-pro"]; after.Records != before.Records {
		t.Fatalf("zero/negative records must not count: %d -> %d", before.Records, after.Records)
	}
}

// TestCalibrationTracker_NilSafety verifies the nil-safe receiver idiom:
// a nil tracker is a disabled tracker.
func TestCalibrationTracker_NilSafety(t *testing.T) {
	var tr *CalibrationTracker
	tr.RecordEstimate("m", 10)
	tr.RecordActual("m", 10)
	if got := tr.Ratio("m"); got != 1.0 {
		t.Fatalf("nil tracker ratio = %v, want 1.0", got)
	}
	if snap := tr.Snapshot(); len(snap) != 0 {
		t.Fatalf("nil tracker snapshot = %v, want empty", snap)
	}
	if models := tr.sortedModels(); models != nil {
		t.Fatalf("nil tracker models = %v, want nil", models)
	}
}

// TestCalibrationTracker_DecayedWindow verifies old observations decay:
// after the drift flips, the ratio tracks the recent window, not the
// all-time average.
func TestCalibrationTracker_DecayedWindow(t *testing.T) {
	tr := NewCalibrationTracker(CalibrationParams{
		Enabled:         true,
		MinObservations: 100,
		Decay:           0.9, // aggressive: ~10-record window
		ClampMin:        0.5,
		ClampMax:        4.0,
	})
	// History: perfect 1x.
	for i := 0; i < 300; i++ {
		tr.RecordEstimate("m", 100)
		tr.RecordActual("m", 100)
	}
	// Recent: 2x drift dominates the ~10-record window.
	for i := 0; i < 60; i++ {
		tr.RecordEstimate("m", 100)
		tr.RecordActual("m", 200)
	}
	if got := tr.Ratio("m"); math.Abs(got-2.0) > 0.1 {
		t.Fatalf("decayed ratio = %v, want ~2.0 (recent window dominates)", got)
	}
}

// TestCalibrationTracker_SnapshotDeterministic pins the /statsz shape:
// tracked models (sorted via the test helper), calibrated flag on the
// warm-up boundary.
func TestCalibrationTracker_SnapshotDeterministic(t *testing.T) {
	tr := NewCalibrationTracker(CalibrationParams{Enabled: true, MinObservations: 10, Decay: 0.99, ClampMin: 0.5, ClampMax: 4.0})
	tr.RecordEstimate("b-model", 5) // below warm-up
	tr.RecordEstimate("a-model", 100)
	tr.RecordActual("a-model", 150)
	snap := tr.Snapshot()
	if models := tr.sortedModels(); len(models) != 2 || models[0] != "a-model" || models[1] != "b-model" {
		t.Fatalf("sorted models = %v", models)
	}
	if !snap["a-model"].Calibrated {
		t.Error("a-model crossed the warm-up floor, must be calibrated")
	}
	if snap["b-model"].Calibrated {
		t.Error("b-model is below the warm-up floor, must not be calibrated")
	}
	if got := snap["a-model"].Ratio; math.Abs(got-1.5) > 0.05 {
		t.Errorf("a-model ratio = %v, want ~1.5", got)
	}
}

// sortedModels returns the tracked model names in stable order (test-only
// view of the tracker's map; production /statsz consumers read Snapshot).
func (t *CalibrationTracker) sortedModels() []string {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	names := make([]string, 0, len(t.models))
	for name := range t.models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
