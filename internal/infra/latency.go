package infra

import (
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	latencyMaxSamples   = 100
	latencyMaxKeys      = 500
	latencyStaleTimeout = 1 * time.Hour
	// latencyEvictInterval throttles the stale-key sweep so Record stays O(1)
	// on the hot path: eviction runs at most once per interval instead of
	// scanning every key on every record.
	latencyEvictInterval = latencyStaleTimeout / 12
)

// ModelLatency is a per-(model, provider) latency record: the median of the
// retained sample buffer, its size, and the last update time. Entries are
// immutable once published by the tracker.
type ModelLatency struct {
	Model      string
	Provider   string
	MedianMs   float64
	SampleSize int
	LastUpdate time.Time
}

// LatencyTracker maintains per-(model, provider) latency sample buffers with
// incremental sorted insertion and median computation, used for latency-based
// target reordering. Entries are immutable once published: Record never
// mutates an entry in place, so values returned by Get and Snapshot are
// stable for reading without holding the tracker lock. All methods are
// nil-safe (a nil tracker records nothing, Get reports a miss, Snapshot is
// empty, and SortByLatency returns identity order). Construct with
// NewLatencyTracker; the zero value is not usable.
//
// Keys are the ":"-joined model and provider; provider names are
// config-owned and must not contain ":" so the join is unambiguous. When a
// sample buffer exceeds latencyMaxSamples, the fastest sample is dropped, so
// the retained window skews toward slower observations and the median is
// conservatively biased for reordering.
type LatencyTracker struct {
	mu        sync.RWMutex
	latency   map[string]*ModelLatency
	sorted    map[string][]time.Duration
	lastEvict time.Time
}

// LatencyKey is a model+provider pair used for latency lookups.
type LatencyKey struct {
	Model    string
	Provider string
}

func NewLatencyTracker() *LatencyTracker {
	return &LatencyTracker{
		latency: make(map[string]*ModelLatency),
		sorted:  make(map[string][]time.Duration),
	}
}

// Record adds duration to the per-(model, provider) sample buffer and
// recomputes the median. Buffers are capped at latencyMaxSamples per key and
// keys evicted after latencyStaleTimeout. Caller-supplied model/provider
// strings are cloned on entry creation only (see cloneLabels in metrics.go,
// NENYA-198); updates reuse the existing entry.
func (l *LatencyTracker) Record(model, provider string, duration time.Duration) {
	if l == nil {
		return
	}
	key := model + ":" + provider

	l.mu.Lock()
	defer l.mu.Unlock()

	// Throttled stale-key sweep: at most once per latencyEvictInterval, so
	// the hot path does not scan all keys on every record.
	if now := time.Now(); now.Sub(l.lastEvict) >= latencyEvictInterval {
		l.evictStaleLocked()
		l.lastEvict = now
	}
	if len(l.sorted) >= latencyMaxKeys {
		if _, exists := l.sorted[key]; !exists {
			return
		}
	}

	buf, ok := l.sorted[key]
	if !ok {
		buf = make([]time.Duration, 0, latencyMaxSamples)
	}

	buf = insertSorted(buf, duration)

	if len(buf) > latencyMaxSamples {
		trimmed := make([]time.Duration, latencyMaxSamples)
		copy(trimmed, buf[1:])
		buf = trimmed
	}

	l.sorted[key] = buf
	// Clone caller-supplied strings only on entry creation (NENYA-198) and
	// publish a fresh ModelLatency per record: entries handed out by
	// Get/Snapshot must never be mutated in place, or concurrent readers of a
	// previously returned snapshot observe torn field sets (data race).
	ml, ok := l.latency[key]
	if !ok {
		ml = &ModelLatency{
			Model:    strings.Clone(model),
			Provider: strings.Clone(provider),
		}
	} else {
		prev := *ml
		ml = &prev
	}
	ml.MedianMs = float64(buf[len(buf)/2].Milliseconds())
	ml.SampleSize = len(buf)
	ml.LastUpdate = time.Now()
	l.latency[key] = ml
}

// insertSorted inserts a duration into a sorted slice using binary search.
// The returned slice is guaranteed to be sorted.
func insertSorted(sorted []time.Duration, val time.Duration) []time.Duration {
	lo, hi := 0, len(sorted)
	for lo < hi {
		mid := (lo + hi) / 2
		if sorted[mid] < val {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	sorted = append(sorted, 0)
	copy(sorted[lo+1:], sorted[lo:])
	sorted[lo] = val
	return sorted
}

// evictStaleLocked removes latency entries that have not been updated
// within the stale timeout (1 hour). Caller must hold l.mu.
func (l *LatencyTracker) evictStaleLocked() {
	cutoff := time.Now().Add(-latencyStaleTimeout)
	for key, ml := range l.latency {
		if ml.LastUpdate.Before(cutoff) {
			delete(l.latency, key)
			delete(l.sorted, key)
		}
	}
}

// Get returns the latency record for the (model, provider) pair; ok is false
// when the pair is unobserved. Published entries are immutable (see
// LatencyTracker), so the returned value is stable for reading without the
// lock; callers must not mutate it.
func (l *LatencyTracker) Get(model, provider string) (*ModelLatency, bool) {
	if l == nil {
		return nil, false
	}
	key := model + ":" + provider

	l.mu.RLock()
	defer l.mu.RUnlock()

	latency, ok := l.latency[key]
	return latency, ok
}

// Snapshot returns all latency records keyed by "model:provider". Published
// entries are immutable (see LatencyTracker); callers must not mutate them.
func (l *LatencyTracker) Snapshot() map[string]*ModelLatency {
	if l == nil {
		return map[string]*ModelLatency{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()

	snapshot := make(map[string]*ModelLatency, len(l.latency))
	for k, v := range l.latency {
		snapshot[k] = v
	}
	return snapshot
}

const latencyJitterRange = 0.10 // Full range (0.10), halved by formula: ±latencyJitterRange/2 = ±0.05 = ±5%

// SortByLatency sorts keys by median latency from this tracker,
// applying ±5% random jitter to prevent thundering herd.
// Returns the indices of the sorted keys. Targets without latency
// data are placed last.
func (l *LatencyTracker) SortByLatency(keys []LatencyKey, jitterFn func() float64) []int {
	if l == nil || len(keys) <= 1 {
		indices := make([]int, len(keys))
		for i := range keys {
			indices[i] = i
		}
		return indices
	}

	if jitterFn == nil {
		jitterFn = rand.Float64
	}

	indices := make([]int, len(keys))
	for i := range keys {
		indices[i] = i
	}

	sort.SliceStable(indices, func(i, j int) bool {
		latencyI, okI := l.Get(keys[indices[i]].Model, keys[indices[i]].Provider)
		latencyJ, okJ := l.Get(keys[indices[j]].Model, keys[indices[j]].Provider)

		if !okI && !okJ {
			return false
		}
		if !okI {
			return false
		}
		if !okJ {
			return true
		}

		jitterI := latencyI.MedianMs * (1.0 + (jitterFn()*latencyJitterRange - latencyJitterRange/2))
		jitterJ := latencyJ.MedianMs * (1.0 + (jitterFn()*latencyJitterRange - latencyJitterRange/2))

		return jitterI < jitterJ
	})

	return indices
}
