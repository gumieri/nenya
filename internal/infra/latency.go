package infra

import (
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
	samples map[string][]time.Duration
}

func NewLatencyTracker() *LatencyTracker {
	return &LatencyTracker{
		latency: make(map[string]*ModelLatency),
		samples: make(map[string][]time.Duration),
	}
}

func (l *LatencyTracker) Record(model, provider string, duration time.Duration) {
	key := model + ":" + provider

	l.mu.Lock()
	defer l.mu.Unlock()

	l.evictStaleLocked()
	if len(l.samples) >= latencyMaxKeys {
		if _, exists := l.samples[key]; !exists {
			return
		}
	}

	if l.samples[key] == nil {
		l.samples[key] = make([]time.Duration, 0, latencyMaxSamples)
	}

	l.samples[key] = append(l.samples[key], duration)

	if len(l.samples[key]) > latencyMaxSamples {
		trimmed := make([]time.Duration, latencyMaxSamples)
		copy(trimmed, l.samples[key][len(l.samples[key])-latencyMaxSamples:])
		l.samples[key] = trimmed
	}

	l.updateMedianLocked(key, model, provider)
}

func (l *LatencyTracker) evictStaleLocked() {
	cutoff := time.Now().Add(-latencyStaleTimeout)
	for key, ml := range l.latency {
		if ml.LastUpdate.Before(cutoff) {
			delete(l.latency, key)
			delete(l.samples, key)
		}
	}
}

func (l *LatencyTracker) updateMedianLocked(key, model, provider string) {
	samples := l.samples[key]
	if len(samples) == 0 {
		return
	}

	n := len(samples)
	sorted := make([]time.Duration, n)
	copy(sorted, samples)

	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	median := sorted[n/2]

	l.latency[key] = &ModelLatency{
		Model:      model,
		Provider:   provider,
		MedianMs:   float64(median.Milliseconds()),
		SampleSize: n,
		LastUpdate: time.Now(),
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
