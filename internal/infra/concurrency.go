package infra

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// ConcurrencyLimiter enforces per-key limits on the number of in-flight
// requests, queueing excess acquires until a slot frees or the caller's
// context is canceled. It is the admission-control primitive used for
// providers that cap concurrent (not per-minute) request volume — e.g. the
// Z.AI GLM Coding Plan, which enforces a per-model concurrency limit.
//
// Keys are composite, conventionally "provider/model". Limits are resolved
// by the caller and supplied to Acquire, so configuration changes (SIGHUP
// reload) take effect on the next acquire without restarting the limiter.
//
// Thread-safety: all methods are safe for concurrent use. A slot is granted
// by sending a token into a buffered channel; the caller must invoke the
// returned release function exactly once when the request finishes.
type ConcurrencyLimiter struct {
	mu    sync.RWMutex
	slots map[string]*concSlot
}

// concSlot is one semaphore (buffered channel of size = limit) plus the last
// time a slot was granted or released, used for idle pruning. lastUsed is an
// atomic (UnixNano) so concurrent grant/release calls do not race.
type concSlot struct {
	sem      chan struct{}
	lastUsed atomic.Int64
}

// NewConcurrencyLimiter creates a limiter with no registered keys. Keys are
// created lazily on first acquire and pruned after they go idle.
func NewConcurrencyLimiter() *ConcurrencyLimiter {
	return &ConcurrencyLimiter{slots: make(map[string]*concSlot)}
}

// concurrencyIdleThreshold is how long a key may go without activity before
// it is eligible for pruning on the next acquire, mirroring the rate-limit
// bucket eviction policy.
const concurrencyIdleThreshold = 5 * time.Minute

// Acquire blocks until a slot for key is free or ctx is canceled. limit <= 0
// means unlimited (immediate no-op release, no slot created). On success it
// returns a release function that frees the slot; the function is
// idempotent (safe to call more than once). When ctx is canceled while
// waiting, Acquire returns ctx.Err() and no release is issued.
func (c *ConcurrencyLimiter) Acquire(ctx context.Context, key string, limit int) (func(), error) {
	if c == nil || limit <= 0 {
		return func() {}, nil
	}

	slot := c.getOrCreateSlot(key, limit)

	select {
	case slot.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	slot.markUsed()
	return sync.OnceFunc(func() {
		<-slot.sem
		slot.markUsed()
	}), nil
}

// getOrCreateSlot returns the semaphore for key, creating it with the given
// capacity if absent. Callers hold no lock, so a concurrent create for the
// same key may win; the loser's channel is simply discarded (no slots were
// granted on it yet, so nothing leaks).
func (c *ConcurrencyLimiter) getOrCreateSlot(key string, limit int) *concSlot {
	c.mu.RLock()
	if s, ok := c.slots[key]; ok {
		c.mu.RUnlock()
		return s
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.slots[key]; ok {
		return s
	}
	if len(c.slots) >= maxConcurrencyKeys {
		evictIdleConcurrencySlotsLocked(c)
	}
	s := &concSlot{sem: make(chan struct{}, limit)}
	s.lastUsed.Store(time.Now().UnixNano())
	c.slots[key] = s
	return s
}

// maxConcurrencyKeys bounds the number of concurrently tracked keys,
// matching maxRateLimitHosts.
const maxConcurrencyKeys = 100

// evictIdleConcurrencySlotsLocked removes keys that have been idle past the
// threshold. Caller must hold c.mu.
func evictIdleConcurrencySlotsLocked(c *ConcurrencyLimiter) {
	now := time.Now()
	for k, s := range c.slots {
		if len(s.sem) == 0 && now.Sub(time.Unix(0, s.lastUsed.Load())) > concurrencyIdleThreshold {
			delete(c.slots, k)
		}
	}
}

// markUsed records activity time (grant or release) for a slot.
func (s *concSlot) markUsed() {
	s.lastUsed.Store(time.Now().UnixNano())
}

// Inflight returns the number of currently held slots for key.
func (c *ConcurrencyLimiter) Inflight(key string) int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.slots[key]
	if !ok {
		return 0
	}
	return len(s.sem)
}

// Snapshot returns the current in-flight count for every tracked key.
func (c *ConcurrencyLimiter) Snapshot() map[string]int {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]int, len(c.slots))
	for k, s := range c.slots {
		out[k] = len(s.sem)
	}
	return out
}
