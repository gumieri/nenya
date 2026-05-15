package resilience

import (
	"math"
	"math/rand"
	"sync"
	"time"
)

const (
	authCooldown    = 2 * time.Minute
	serverCooldown  = 1 * time.Second
	unknownCooldown = 30 * time.Second

	rateLimitBaseMs = 500
	quotaBaseMs     = 60000
	capacityBaseMs  = 30000
	maxBackoffLevel = 15

	// backoffJitterRange is the fraction of the base duration to apply as jitter.
	// A value of 0.10 means ±5% jitter (total range of 10%).
	backoffJitterRange = 0.10 // ±5% jitter to prevent thundering herd
)

// computeExponentialBackoffWithJitter calculates cooldown with exponential backoff
// and applies ±5% random jitter to prevent thundering herd when multiple models
// hit backoff simultaneously.
func computeExponentialBackoffWithJitter(level int, baseMs int64) time.Duration {
	cooldown := computeExponentialBackoff(level, baseMs)

	// Apply ±5% jitter
	jitter := float64(cooldown) * (backoffJitterRange / 2) * (2*rand.Float64() - 1)
	jittered := float64(cooldown) + jitter

	// Clamp to non-negative
	if jittered < 0 {
		jittered = 0
	}

	return time.Duration(jittered)
}

// computeExponentialBackoff calculates cooldown: baseMs * 2^level, capped at max.
// This is the base function without jitter. Use computeExponentialBackoffWithJitter
// for production code to add jitter and prevent thundering herd.
func computeExponentialBackoff(level int, baseMs int64) time.Duration {
	if level > maxBackoffLevel {
		level = maxBackoffLevel
	}
	if level < 0 {
		level = 0
	}

	ms := float64(baseMs) * math.Pow(2, float64(level))
	maxDuration := float64(math.MaxInt64 / int64(time.Millisecond))
	if ms >= maxDuration {
		ms = maxDuration - 1
	}

	return time.Duration(ms) * time.Millisecond
}

// BackoffTracker tracks exponential backoff levels per circuit key.
// Each key (agent:provider:model) has an independent level that advances
// on retriable failures and resets on success. Level is capped at
// maxBackoffLevel (15) to prevent unbounded growth.
//
// Thread-safety: All methods are safe to call concurrently. Internal state
// is protected by a sync.Mutex. The optional onIncrement callback is
// invoked while holding the mutex — see WARNING on SetBackoffIncrementCallback.
type BackoffTracker struct {
	mu          sync.Mutex
	levels      map[string]int
	onIncrement func(key string, level int)
}

// NewBackoffTracker creates a new BackoffTracker.
func NewBackoffTracker() *BackoffTracker {
	return &BackoffTracker{levels: make(map[string]int)}
}

// NewBackoffTrackerWithCallback creates a new BackoffTracker with a callback
// that is invoked when the backoff level increments.
func NewBackoffTrackerWithCallback(onIncrement func(key string, level int)) *BackoffTracker {
	return &BackoffTracker{
		levels:      make(map[string]int),
		onIncrement: onIncrement,
	}
}

// GetLevel returns the current backoff level for a model.
func (bt *BackoffTracker) GetLevel(model string) int {
	if bt == nil {
		return 0
	}
	bt.mu.Lock()
	defer bt.mu.Unlock()
	return bt.levels[model]
}

// Increment increments the backoff level for a model and returns the new level.
func (bt *BackoffTracker) Increment(model string) int {
	if bt == nil {
		return 0
	}
	bt.mu.Lock()
	defer bt.mu.Unlock()
	bt.levels[model]++
	if bt.levels[model] > maxBackoffLevel {
		bt.levels[model] = maxBackoffLevel
	}
	newLevel := bt.levels[model]

	if bt.onIncrement != nil {
		bt.onIncrement(model, newLevel)
	}

	return newLevel
}

// Reset clears the backoff level for a model.
func (bt *BackoffTracker) Reset(model string) {
	if bt == nil {
		return
	}
	bt.mu.Lock()
	defer bt.mu.Unlock()
	delete(bt.levels, model)
}
