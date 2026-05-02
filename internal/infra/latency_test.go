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