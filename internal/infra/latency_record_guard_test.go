package infra

import (
	"testing"
	"time"
)

// TestLatencyRecordRejectsNonPositiveDurations pins AUD-72 (NENYA-277):
// Record must carry the same duration guard as RecordTTFT — a zero or
// negative duration is an unmeasured call, not a fast one. Pre-fix a
// caller without a measurement (a zero-value timer, a clock going
// backwards) poisoned the sample buffer and dragged the median down,
// distorting the latency-aware reorder and the warm-cache tie-break.
func TestLatencyRecordRejectsNonPositiveDurations(t *testing.T) {
	l := NewLatencyTracker()

	l.Record("m", "p", 0)
	l.Record("m", "p", -time.Second)
	if _, ok := l.Get("m", "p"); ok {
		t.Fatal("zero/negative durations must not create a latency entry")
	}

	l.Record("m", "p", 100*time.Millisecond)
	got, ok := l.Get("m", "p")
	if !ok || got.SampleSize != 1 || got.MedianMs != 100 {
		t.Fatalf("a positive duration must still record: got %+v ok=%v", got, ok)
	}

	// A later zero must not drag the median or inflate the sample count.
	l.Record("m", "p", 0)
	got, _ = l.Get("m", "p")
	if got.SampleSize != 1 || got.MedianMs != 100 {
		t.Fatalf("zero durations must be rejected: %+v", got)
	}
}
