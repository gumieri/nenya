package infra

import (
	"slices"
	"testing"
	"time"
)

func TestSortByLatency_NilTracker(t *testing.T) {
	keys := []LatencyKey{{Model: "a"}, {Model: "b"}, {Model: "c"}}
	indices := (*LatencyTracker)(nil).SortByLatency(keys, nil)
	if len(indices) != 3 {
		t.Fatalf("expected 3, got %d", len(indices))
	}
	for i, idx := range indices {
		if idx != i {
			t.Fatalf("expected identity order, got %d at position %d", idx, i)
		}
	}
}

func TestSortByLatency_EmptySlice(t *testing.T) {
	tracker := NewLatencyTracker()
	indices := tracker.SortByLatency([]LatencyKey{}, nil)
	if len(indices) != 0 {
		t.Fatalf("expected 0, got %d", len(indices))
	}
}

func TestSortByLatency_SingleTarget(t *testing.T) {
	tracker := NewLatencyTracker()
	keys := []LatencyKey{{Model: "model-a", Provider: "p1"}}
	indices := tracker.SortByLatency(keys, nil)
	if len(indices) != 1 || indices[0] != 0 {
		t.Fatal("single target should return identity index")
	}
}

func TestSortByLatency_NoLatencyData(t *testing.T) {
	tracker := NewLatencyTracker()
	keys := []LatencyKey{
		{Model: "model-a", Provider: "p"},
		{Model: "model-b", Provider: "p"},
	}

	indices := tracker.SortByLatency(keys, nil)

	if len(indices) != 2 {
		t.Fatalf("expected 2, got %d", len(indices))
	}
	if !slices.Equal(indices, []int{0, 1}) {
		t.Fatal("order should be preserved when no latency data exists")
	}
}

func TestSortByLatency_TwoTargets_DifferentLatency(t *testing.T) {
	tracker := NewLatencyTracker()
	tracker.Record("fast-model", "p1", 100*time.Millisecond)
	tracker.Record("fast-model", "p1", 120*time.Millisecond)
	tracker.Record("slow-model", "p1", 500*time.Millisecond)
	tracker.Record("slow-model", "p1", 480*time.Millisecond)

	keys := []LatencyKey{
		{Model: "slow-model", Provider: "p1"},
		{Model: "fast-model", Provider: "p1"},
	}

	indices := tracker.SortByLatency(keys, nil)

	if len(indices) != 2 {
		t.Fatalf("expected 2, got %d", len(indices))
	}
	// indices[0] should point to the fast model (index 1 in original list)
	if indices[0] != 1 {
		t.Errorf("expected fast-model first (index 1), got index %d", indices[0])
	}
	if indices[1] != 0 {
		t.Errorf("expected slow-model second (index 0), got index %d", indices[1])
	}
}

func TestSortByLatency_JitterFnInjected(t *testing.T) {
	tracker := NewLatencyTracker()
	tracker.Record("fast", "p1", 100*time.Millisecond)
	tracker.Record("slow", "p1", 200*time.Millisecond)

	keys := []LatencyKey{
		{Model: "fast", Provider: "p1"},
		{Model: "slow", Provider: "p1"},
	}

	deterministicJitter := func() float64 { return 0 }
	indices := tracker.SortByLatency(keys, deterministicJitter)

	if indices[0] != 0 {
		t.Errorf("expected fast first with jitter=0, got index %d", indices[0])
	}
}

func TestSortByLatency_NilJitterFn(t *testing.T) {
	tracker := NewLatencyTracker()
	tracker.Record("model-a", "p1", 50*time.Millisecond)
	tracker.Record("model-b", "p1", 100*time.Millisecond)

	keys := []LatencyKey{
		{Model: "model-a", Provider: "p1"},
		{Model: "model-b", Provider: "p1"},
	}

	indices := tracker.SortByLatency(keys, nil)
	if len(indices) != 2 {
		t.Fatalf("expected 2, got %d", len(indices))
	}
}

func TestSortByLatency_OnlySomeHaveLatency(t *testing.T) {
	tracker := NewLatencyTracker()
	tracker.Record("tracked", "p1", 100*time.Millisecond)

	keys := []LatencyKey{
		{Model: "tracked", Provider: "p1"},
		{Model: "unknown", Provider: "p1"},
	}

	indices := tracker.SortByLatency(keys, nil)

	if len(indices) != 2 {
		t.Fatalf("expected 2, got %d", len(indices))
	}
	if indices[0] != 0 {
		t.Errorf("expected tracked (index 0) first, got index %d", indices[0])
	}
	if indices[1] != 1 {
		t.Errorf("expected unknown (index 1) second, got index %d", indices[1])
	}
}

// TestLatencyTracker_RecordConcurrentWithReaders exercises Record against
// concurrent Get/Snapshot readers under -race, guarding the lock discipline
// of all three paths. Published values are the immutable entries stored at
// insertion (Record never mutates an entry in place); Get/Snapshot hand out
// that shared read-only entry, so callers must not mutate what they receive.
func TestLatencyTracker_RecordConcurrentWithReaders(t *testing.T) {
	l := NewLatencyTracker()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			l.Record("model", "provider", time.Duration(i+1)*time.Millisecond)
		}
	}()
	for i := 0; i < 500; i++ {
		if ml, ok := l.Get("model", "provider"); ok {
			_ = ml.MedianMs + float64(ml.SampleSize) + float64(ml.LastUpdate.UnixNano())
		}
		snap := l.Snapshot()
		for _, ml := range snap {
			_ = ml.MedianMs + float64(ml.SampleSize) + float64(ml.LastUpdate.UnixNano())
		}
	}
	<-done
	if len(l.Snapshot()) != 1 {
		t.Fatalf("snapshot entries = %d, want 1", len(l.Snapshot()))
	}
}

// TestLatencyTracker_TTFT pins the time-to-first-effective-output dimension
// (NENYA-178): it is recorded independently of the full-request median, shares
// the sample bounds, is ignored for non-positive observations, and is exposed
// through Get/Snapshot (the feed for latency-aware decisions and the slow-call
// breaker, NENYA-153).
func TestLatencyTracker_TTFT(t *testing.T) {
	tr := NewLatencyTracker()
	tr.Record("m", "p", 5*time.Second)
	for _, d := range []time.Duration{100 * time.Millisecond, 300 * time.Millisecond, 200 * time.Millisecond} {
		tr.RecordTTFT("m", "p", d)
	}

	ml, ok := tr.Get("m", "p")
	if !ok {
		t.Fatal("no entry for (m, p)")
	}
	if ml.MedianTTFT != 200*time.Millisecond {
		t.Errorf("MedianTTFT = %v, want 200ms (median of 100/200/300)", ml.MedianTTFT)
	}
	if ml.TTFTSampleSize != 3 {
		t.Errorf("TTFTSampleSize = %d, want 3", ml.TTFTSampleSize)
	}
	// The full-request dimension is untouched by TTFT records.
	if ml.MedianMs != 5000 || ml.SampleSize != 1 {
		t.Errorf("full-request record changed: median=%v samples=%d", ml.MedianMs, ml.SampleSize)
	}

	if got, ok := tr.TTFT("m", "p"); !ok || got != 200*time.Millisecond {
		t.Errorf("TTFT(m, p) = %v, %v; want 200ms, true", got, ok)
	}
	if _, ok := tr.TTFT("m", "other"); ok {
		t.Error("TTFT for an unobserved provider must report a miss")
	}

	// Non-positive observations carry no signal.
	tr.RecordTTFT("m", "p", 0)
	tr.RecordTTFT("m", "p", -time.Second)
	if after, _ := tr.Get("m", "p"); after.TTFTSampleSize != 3 {
		t.Errorf("non-positive TTFT was recorded: samples=%d", after.TTFTSampleSize)
	}

	// The sample buffer is capped like the full-request one.
	for i := 0; i < latencyMaxSamples+10; i++ {
		tr.RecordTTFT("m", "p", time.Duration(i+1)*time.Millisecond)
	}
	ml, _ = tr.Get("m", "p")
	if ml.TTFTSampleSize != latencyMaxSamples {
		t.Errorf("TTFTSampleSize = %d, want the cap %d", ml.TTFTSampleSize, latencyMaxSamples)
	}

	// Snapshot carries both dimensions.
	snap := tr.Snapshot()
	if entry, ok := snap["m:p"]; !ok || entry.TTFTSampleSize == 0 {
		t.Errorf("snapshot lost the TTFT dimension: %+v", snap)
	}
}

// TestLatencyTracker_TTFTRemainsNilSafe pins the documented nil contract for the
// new methods.
func TestLatencyTracker_TTFTRemainsNilSafe(t *testing.T) {
	var tr *LatencyTracker
	tr.RecordTTFT("m", "p", time.Second)
	if _, ok := tr.TTFT("m", "p"); ok {
		t.Error("a nil tracker must report a TTFT miss")
	}
}
