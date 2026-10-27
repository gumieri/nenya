package infra

import (
	"testing"
	"time"
)

func TestCacheStateTrackerRecordAndSnapshot(t *testing.T) {
	tr := NewCacheStateTracker()
	tr.Record("m", "p1", 800, 1000)

	snap := tr.Snapshot()
	entry, ok := snap["m:p1"]
	if !ok {
		t.Fatalf("the pair must be snapshotted: %+v", snap)
	}
	if entry.HitRatio < 0.79 || entry.HitRatio > 0.81 {
		t.Fatalf("hit ratio = %v, want ~0.8", entry.HitRatio)
	}
	if entry.InputTokens != 1000 {
		t.Fatalf("input tokens = %v", entry.InputTokens)
	}
	if entry.LastHitAgeSeconds < 0 || entry.LastHitAgeSeconds > 5 {
		t.Fatalf("last-hit age must be fresh: %+v", entry)
	}
	if !entry.Warm {
		t.Fatalf("an 80%% fresh hit must be warm: %+v", entry)
	}
}

// Warm requires BOTH the ratio and freshness; an old hit goes cold.
func TestCacheStateTrackerWarmRequiresFreshness(t *testing.T) {
	tr := NewCacheStateTracker()
	tr.Record("m", "p1", 800, 1000)
	// Age the last hit beyond the window.
	tr.mu.Lock()
	e := tr.entries[LatencyKey{Model: "m", Provider: "p1"}]
	e.lastHitNano.Store(time.Now().Add(-CacheWarmMaxAge - time.Minute).UnixNano())
	tr.mu.Unlock()

	if tr.Warm("m", "p1") {
		t.Fatal("a stale hit must not count as warm")
	}
	snap := tr.Snapshot()["m:p1"]
	if snap.Warm {
		t.Fatalf("the snapshot must agree: %+v", snap)
	}
}

// The reorder tie-break: within the jitter band, warm sorts first; outside
// it, latency stays primary. Nil predicate = plain latency order.
func TestSortByLatencyWithWarmth(t *testing.T) {
	l := NewLatencyTracker()
	// Warm-but-slightly-slower vs cold-but-slightly-faster: within the band.
	l.Record("warm", "p", 100*time.Millisecond)
	l.Record("warm", "p", 100*time.Millisecond)
	l.Record("cold", "p", 103*time.Millisecond)
	l.Record("cold", "p", 103*time.Millisecond)

	warm := func(model, _ string) bool { return model == "warm" }

	// Band-internal: warmth wins (fixed jitter keeps it deterministic; the
	// raw medians 100ms vs 103ms sit inside the ±5% band).
	fixed := func() float64 { return 0.5 }
	keys := []LatencyKey{
		{Model: "cold", Provider: "p"},
		{Model: "warm", Provider: "p"},
	}
	for i := 0; i < 20; i++ {
		got := l.SortByLatencyWithWarmth(keys, fixed, warm, DefaultWarmCacheBand)
		if keys[got[0]].Model != "warm" {
			t.Fatalf("within the band the warm target must lead (iteration %d): %+v", i, got)
		}
	}

	// Outside the band, latency stays primary: a much slower warm target
	// must not jump ahead.
	slow := NewLatencyTracker()
	slow.Record("warm-slow", "p", 900*time.Millisecond)
	slow.Record("warm-slow", "p", 900*time.Millisecond)
	slow.Record("cold-fast", "p", 50*time.Millisecond)
	slow.Record("cold-fast", "p", 50*time.Millisecond)
	keys2 := []LatencyKey{
		{Model: "warm-slow", Provider: "p"},
		{Model: "cold-fast", Provider: "p"},
	}
	got := slow.SortByLatencyWithWarmth(keys2, fixed, warm, DefaultWarmCacheBand)
	if keys2[got[0]].Model != "cold-fast" {
		t.Fatalf("latency stays primary outside the band: %+v", got)
	}

	// nil predicate = the plain latency order.
	// A nil predicate must agree with the plain latency sort, both under a
	// fixed jitter (deterministic).
	keys = []LatencyKey{
		{Model: "cold", Provider: "p"},
		{Model: "warm", Provider: "p"},
	}
	plain := l.SortByLatencyWithWarmth(keys, fixed, nil, DefaultWarmCacheBand)
	withNil := l.SortByLatencyWithWarmth(keys, fixed, nil, DefaultWarmCacheBand)
	for i := range plain {
		if plain[i] != withNil[i] {
			t.Fatalf("a nil predicate must equal the plain latency order: %v vs %v", plain, withNil)
		}
	}
}

// The near-tie band is configurable (NENYA-202 follow-up): the warm-cache
// tie-break applies inside the configured band (as a fraction of the larger
// raw median), not only inside the fixed ±5% jitter band. Band 0 disables
// the tie-break except for exact median ties.
func TestSortByLatencyWithWarmthBandConfigurable(t *testing.T) {
	fixed := func() float64 { return 0.5 }
	l := NewLatencyTracker()
	// 100ms (cold) vs 115ms (warm): a 15% raw-median gap — outside the
	// default ±5% band, inside a 20% band. Warm is deliberately the SLOWER
	// target so the verdict isolates the tie-break from plain latency order.
	l.Record("warm", "p", 115*time.Millisecond)
	l.Record("warm", "p", 115*time.Millisecond)
	l.Record("cold", "p", 100*time.Millisecond)
	l.Record("cold", "p", 100*time.Millisecond)
	warm := func(model, _ string) bool { return model == "warm" }
	keys := []LatencyKey{
		{Model: "cold", Provider: "p"},
		{Model: "warm", Provider: "p"},
	}

	// Default band: latency stays primary (15% > 5%).
	got := l.SortByLatencyWithWarmth(keys, fixed, warm, DefaultWarmCacheBand)
	if keys[got[0]].Model != "cold" {
		t.Fatalf("default band must keep latency primary at a 15%% gap: %+v", got)
	}

	// Widened band: warmth wins at the same gap.
	got = l.SortByLatencyWithWarmth(keys, fixed, warm, 0.20)
	if keys[got[0]].Model != "warm" {
		t.Fatalf("a 20%% band must let warmth win at a 15%% gap: %+v", got)
	}

	// Zero band: no near-tie, even at a 3% gap (warm again the slower one).
	small := NewLatencyTracker()
	small.Record("warm", "p", 103*time.Millisecond)
	small.Record("warm", "p", 103*time.Millisecond)
	small.Record("cold", "p", 100*time.Millisecond)
	small.Record("cold", "p", 100*time.Millisecond)
	keys3 := []LatencyKey{
		{Model: "cold", Provider: "p"},
		{Model: "warm", Provider: "p"},
	}
	got = small.SortByLatencyWithWarmth(keys3, fixed, warm, 0)
	if keys3[got[0]].Model != "cold" {
		t.Fatalf("a zero band must disable the tie-break: %+v", got)
	}
}
