package infra

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- NENYA-190: observed cache warmth per target ---------------------------
//
// The cost model prices cache reads and writes (NENYA-165), but a failover or
// reorder decision that only compares median latency cannot see that one
// target serves the conversation from a warm prompt cache while another would
// re-ingest the whole prefix at first-turn rates. The tracker records the
// observed cache-hit ratio and last-hit age per (model, provider) pair at the
// usage-recording sites, so reorder decisions and /statsz can account for
// cache warmth.

const (
	// CacheWarmRatio is the minimum observed hit ratio for a target to count
	// as warm.
	CacheWarmRatio = 0.5
	// CacheWarmMaxAge bounds how long since the last cache hit a target may
	// be and still count as warm.
	CacheWarmMaxAge = 10 * time.Minute
)

// CacheStateTracker aggregates observed cache warmth per (model, provider)
// pair — the same pair key as the latency tracker, so the reorder path joins
// the two without a mapping. Record is called from the usage-recording sites;
// Snapshot feeds /statsz; Warm answers the reorder tie-break.
type CacheStateTracker struct {
	mu      sync.RWMutex
	entries map[LatencyKey]*cacheStateEntry
}

type cacheStateEntry struct {
	hitTokens   atomic.Uint64
	inputTokens atomic.Uint64
	lastHitNano atomic.Int64 // unix nanos; 0 = never
}

// NewCacheStateTracker returns an empty tracker ready for use.
func NewCacheStateTracker() *CacheStateTracker {
	return &CacheStateTracker{entries: make(map[LatencyKey]*cacheStateEntry)}
}

// Record accumulates one response's cache outcome for the (model, provider)
// pair. A non-positive denominator is ignored (nothing observable); cache-hit
// tokens always refresh the last-hit age. Note: a streaming usage delta may
// carry only the cache dimensions (the total lands in a later chunk), so the
// ratio self-corrects as the request's chunks accumulate.
func (t *CacheStateTracker) Record(model, provider string, cacheHitTokens, totalInputTokens int) {
	if t == nil {
		return
	}
	// Clone the key components before retaining (NENYA-198 pattern): the
	// caller's strings may alias request buffers and the map lives for the
	// process lifetime.
	key := LatencyKey{Model: strings.Clone(model), Provider: strings.Clone(provider)}
	t.mu.Lock()
	entry := t.entries[key]
	if entry == nil {
		entry = &cacheStateEntry{}
		t.entries[key] = entry
	}
	t.mu.Unlock()
	if cacheHitTokens > 0 {
		entry.hitTokens.Add(uint64(cacheHitTokens))
		entry.lastHitNano.Store(time.Now().UnixNano())
	}
	if totalInputTokens > 0 {
		entry.inputTokens.Add(uint64(totalInputTokens))
	}
}

// CacheStateSnapshotEntry is one pair's observed warmth. Model and Provider
// are embedded in every entry because the "model:provider" snapshot key is
// ambiguous when a model id itself contains ':'.
type CacheStateSnapshotEntry struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	// HitRatio is observed cache-hit tokens over observed input tokens
	// (0 when nothing was observed).
	HitRatio float64 `json:"hit_ratio"`
	// InputTokens is the observed input-token total behind the ratio.
	InputTokens uint64 `json:"input_tokens"`
	// LastHitAgeSeconds is the age of the last cache hit; -1 when never.
	LastHitAgeSeconds float64 `json:"last_hit_age_seconds"`
	// Warm reports whether the pair counts as warm for the reorder
	// tie-break (hit ratio >= CacheWarmRatio and last hit within
	// CacheWarmMaxAge).
	Warm bool `json:"warm"`
}

// Snapshot returns the per-pair warmth, keyed "model:provider" (the same
// join convention as the latency section). Nil-safe; empty when nothing was
// observed.
func (t *CacheStateTracker) Snapshot() map[string]CacheStateSnapshotEntry {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]CacheStateSnapshotEntry, len(t.entries))
	for key, e := range t.entries {
		hits := e.hitTokens.Load()
		input := e.inputTokens.Load()
		entry := CacheStateSnapshotEntry{
			Model:             key.Model,
			Provider:          key.Provider,
			LastHitAgeSeconds: -1,
		}
		if input > 0 {
			entry.HitRatio = float64(hits) / float64(input)
			entry.InputTokens = input
		}
		if last := e.lastHitNano.Load(); last != 0 {
			age := time.Since(time.Unix(0, last))
			entry.LastHitAgeSeconds = age.Seconds()
		}
		entry.Warm = entry.HitRatio >= CacheWarmRatio &&
			entry.LastHitAgeSeconds >= 0 && ageWithin(entry.LastHitAgeSeconds, CacheWarmMaxAge)
		out[fmt.Sprintf("%s:%s", key.Model, key.Provider)] = entry
	}
	return out
}

// Warm reports whether the (model, provider) pair counts as warm for the
// reorder tie-break: observed hit ratio at or above CacheWarmRatio with the
// last hit inside CacheWarmMaxAge. Nil-safe (cold).
func (t *CacheStateTracker) Warm(model, provider string) bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	e := t.entries[LatencyKey{Model: model, Provider: provider}]
	t.mu.RUnlock()
	if e == nil {
		return false
	}
	hits := e.hitTokens.Load()
	input := e.inputTokens.Load()
	if input == 0 || float64(hits)/float64(input) < CacheWarmRatio {
		return false
	}
	last := e.lastHitNano.Load()
	if last == 0 {
		return false
	}
	return ageWithin(time.Since(time.Unix(0, last)).Seconds(), CacheWarmMaxAge)
}

func ageWithin(ageSeconds float64, max time.Duration) bool {
	return ageSeconds <= max.Seconds()
}
