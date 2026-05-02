package infra

import (
	"math/rand"
	"sort"
	"sync"
	"time"
)

const (
	latencyMaxSamples   = 100
	latencyMaxKeys      = 500
	latencyStaleTimeout = 1 * time.Hour
)

type ModelLatency struct {
	Model      string
	Provider   string
	MedianMs   float64
	SampleSize int
	LastUpdate time.Time
}

type LatencyTracker struct {
	mu      sync.RWMutex
	latency map[string]*ModelLatency
	sorted  map[string][]time.Duration
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

func (l *LatencyTracker) Record(model, provider string, duration time.Duration) {
	key := model + ":" + provider

	l.mu.Lock()
	defer l.mu.Unlock()

	l.evictStaleLocked()
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
	l.latency[key] = &ModelLatency{
		Model:      model,
		Provider:   provider,
		MedianMs:   float64(buf[len(buf)/2].Milliseconds()),
		SampleSize: len(buf),
		LastUpdate: time.Now(),
	}
}

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

func (l *LatencyTracker) evictStaleLocked() {
	cutoff := time.Now().Add(-latencyStaleTimeout)
	for key, ml := range l.latency {
		if ml.LastUpdate.Before(cutoff) {
			delete(l.latency, key)
			delete(l.sorted, key)
		}
	}
}

func (l *LatencyTracker) Get(model, provider string) (*ModelLatency, bool) {
	key := model + ":" + provider

	l.mu.RLock()
	defer l.mu.RUnlock()

	latency, ok := l.latency[key]
	return latency, ok
}

func (l *LatencyTracker) Snapshot() map[string]*ModelLatency {
	l.mu.RLock()
	defer l.mu.RUnlock()

	snapshot := make(map[string]*ModelLatency, len(l.latency))
	for k, v := range l.latency {
		snapshot[k] = v
	}
	return snapshot
}

const latencyJitterPct = 0.10

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

		jitterI := latencyI.MedianMs * (1.0 + (jitterFn()*latencyJitterPct - latencyJitterPct/2))
		jitterJ := latencyJ.MedianMs * (1.0 + (jitterFn()*latencyJitterPct - latencyJitterPct/2))

		return jitterI < jitterJ
	})

	return indices
}
