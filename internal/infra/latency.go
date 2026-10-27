package infra

import (
	"math"
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

// ModelLatency is a per-(model, provider) latency record with two dimensions:
// the upstream response-header latency (MedianMs/SampleSize) and the
// time-to-first-effective-output of streaming requests
// (MedianTTFT/TTFTSampleSize, NENYA-178). Entries are immutable once published
// by the tracker.
type ModelLatency struct {
	Model      string
	Provider   string
	MedianMs   float64
	SampleSize int
	LastUpdate time.Time

	// MedianTTFT (NENYA-178) is the exact median time-to-first-effective-output
	// and TTFTSampleSize its sample count, recorded only on streaming paths.
	// Zero when the pair has no streaming sample yet. It is kept as a
	// time.Duration (not a millisecond float) so the TTFT lookup is lossless for
	// sub-millisecond medians; the /statsz rendering converts to milliseconds.
	// This is the clock the slow-call circuit breaker consumes (NENYA-153): a
	// slow first token is a degraded target even when the full request
	// eventually succeeds.
	MedianTTFT     time.Duration
	TTFTSampleSize int
}

// LatencyTracker maintains two per-(model, provider) sample buffers with
// incremental sorted insertion and median computation: the upstream
// response-header latency (Record) and the time-to-first-effective-output of
// streaming requests (RecordTTFT, NENYA-178), used for latency-based target
// reordering and for the slow-call circuit breaker. Entries are immutable once
// published: neither Record nor RecordTTFT mutates an entry in place, so values
// returned by Get and Snapshot are stable for reading without holding the
// tracker lock. All methods are nil-safe (a nil tracker records nothing, Get
// reports a miss, Snapshot is empty, and SortByLatency returns identity order).
// Construct with NewLatencyTracker; the zero value is not usable.
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
	ttft      map[string][]time.Duration
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
		ttft:    make(map[string][]time.Duration),
	}
}

// Record adds duration to the per-(model, provider) sample buffer and
// recomputes the median. Buffers are capped at latencyMaxSamples per key and
// keys evicted after latencyStaleTimeout. Caller-supplied model/provider
// strings are cloned on entry creation only (see cloneLabels in metrics.go,
// NENYA-198); updates reuse the existing entry.
func (l *LatencyTracker) Record(model, provider string, duration time.Duration) {
	if l == nil || duration <= 0 {
		return
	}
	key := model + ":" + provider

	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.admitLocked(key) {
		return
	}

	buf := sampleLocked(l.sorted, key, duration)
	l.sorted[key] = buf

	ml := l.publishLocked(key, model, provider)
	ml.MedianMs = float64(buf[len(buf)/2].Milliseconds())
	ml.SampleSize = len(buf)
	l.latency[key] = ml
}

// RecordTTFT adds a time-to-first-effective-output observation (NENYA-178) for
// the (model, provider) pair and recomputes its median. It shares the sample
// buffer bounds, the stale-key sweep and the key cap with Record, and updates
// the same published entry (the full-request median and the TTFT median are two
// dimensions of one pair). A non-positive duration is ignored: it carries no
// signal and would drag the median toward zero.
//
// Callers on the request path record the header latency (Record) before the
// first output arrives, so a published entry normally carries both dimensions;
// if RecordTTFT were the only writer, MedianMs/SampleSize would stay zero and
// SortByLatency would read the pair as the fastest target. /statsz omits an
// unobserved dimension rather than reporting a zero.
func (l *LatencyTracker) RecordTTFT(model, provider string, duration time.Duration) {
	if l == nil || duration <= 0 {
		return
	}
	key := model + ":" + provider

	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.admitLocked(key) {
		return
	}

	buf := sampleLocked(l.ttft, key, duration)
	l.ttft[key] = buf

	ml := l.publishLocked(key, model, provider)
	ml.MedianTTFT = buf[len(buf)/2]
	ml.TTFTSampleSize = len(buf)
	l.latency[key] = ml
}

// TTFT returns the median time-to-first-effective-output for the pair; ok is
// false when no streaming sample has been recorded.
//
// This is the feed for the slow-call circuit breaker and first-token-aware
// routing (NENYA-153), which consumes it when it lands; today the value is
// recorded, exposed through Get/Snapshot (/statsz) and exported as the
// nenya_ttft_seconds histogram. The buffered MCP replay path does not record
// (it never streams upstream), so a model reached only through it has no TTFT
// sample.
func (l *LatencyTracker) TTFT(model, provider string) (time.Duration, bool) {
	if l == nil {
		return 0, false
	}
	key := model + ":" + provider

	l.mu.RLock()
	defer l.mu.RUnlock()

	ml, ok := l.latency[key]
	if !ok || ml.TTFTSampleSize == 0 {
		return 0, false
	}
	return ml.MedianTTFT, true
}

// admitLocked runs the throttled stale-key sweep and enforces the key cap,
// reporting whether the key may be recorded.
func (l *LatencyTracker) admitLocked(key string) bool {
	// Throttled stale-key sweep: at most once per latencyEvictInterval, so
	// the hot path does not scan all keys on every record.
	if now := time.Now(); now.Sub(l.lastEvict) >= latencyEvictInterval {
		l.evictStaleLocked()
		l.lastEvict = now
	}
	// l.latency is the authoritative map: both sample buffers are written
	// together with it, so its length is the key count.
	if len(l.latency) >= latencyMaxKeys {
		if _, exists := l.latency[key]; !exists {
			return false
		}
	}
	return true
}

// sampleLocked inserts duration into the per-key buffer of bufs, keeping it
// sorted and capped at latencyMaxSamples (the fastest sample is dropped, so the
// retained window skews toward slower observations). Callers hold l.mu.
func sampleLocked(bufs map[string][]time.Duration, key string, duration time.Duration) []time.Duration {
	buf, ok := bufs[key]
	if !ok {
		buf = make([]time.Duration, 0, latencyMaxSamples)
	}
	buf = insertSorted(buf, duration)
	if len(buf) > latencyMaxSamples {
		trimmed := make([]time.Duration, latencyMaxSamples)
		copy(trimmed, buf[1:])
		buf = trimmed
	}
	return buf
}

// publishLocked returns the entry to mutate for key: a fresh copy of the
// existing entry, or a new one with cloned labels. Clone caller-supplied
// strings only on entry creation (NENYA-198) and never mutate a published entry
// in place, or concurrent readers of a previously returned snapshot observe
// torn field sets (data race).
func (l *LatencyTracker) publishLocked(key, model, provider string) *ModelLatency {
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
	ml.LastUpdate = time.Now()
	return ml
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
			delete(l.ttft, key)
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

// DefaultWarmCacheBand is the default width of the NENYA-190 near-tie band,
// as a fraction of the larger raw median: ±5%. Configurable via
// governance.auto_reorder_warm_cache_band_pct (NENYA-202 follow-up).
const DefaultWarmCacheBand = latencyJitterRange / 2

// SortByLatency sorts keys by median latency from this tracker,
// applying ±5% random jitter to prevent thundering herd.
// Returns the indices of the sorted keys. Targets without latency
// data are placed last.
func (l *LatencyTracker) SortByLatency(keys []LatencyKey, jitterFn func() float64) []int {
	return l.SortByLatencyWithWarmth(keys, jitterFn, nil, DefaultWarmCacheBand)
}

// SortByLatencyWithWarmth is SortByLatency with an optional cache-warmth
// tie-break (NENYA-190): when two keys' RAW medians are within the warmBand
// fraction (default ±5%, configurable — NENYA-202 follow-up) and a warmth
// predicate says one is warm, the warm key comes first — a cold-cache switch
// re-ingests the whole prefix at first-turn rates, so the warm target serves
// the conversation cheaper at indistinguishable latency. warm may be nil (no
// tie-break). A warmBand of 0 disables the tie-break except for exact median
// ties; negative values are treated as 0.
func (l *LatencyTracker) SortByLatencyWithWarmth(keys []LatencyKey, jitterFn func() float64, warm func(model, provider string) bool, warmBand float64) []int {
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

		// NENYA-190 warmth tie-break: only inside the band where the jitter
		// would make the latency comparison noise anyway. Note: across 3+
		// targets the band+warmth+jitter combination can be intransitive for
		// one request; sort.SliceStable tolerates it (unspecified order for
		// that request), and fresh jitter per comparison is the pre-existing
		// behavior.
		if warm != nil {
			wi := warm(keys[indices[i]].Model, keys[indices[i]].Provider)
			wj := warm(keys[indices[j]].Model, keys[indices[j]].Provider)
			if wi != wj {
				band := warmBand
				if band < 0 {
					band = 0
				}
				if math.Abs(latencyI.MedianMs-latencyJ.MedianMs) <= band*math.Max(latencyI.MedianMs, latencyJ.MedianMs) {
					return wi
				}
			}
		}

		jitterI := latencyI.MedianMs * (1.0 + (jitterFn()*latencyJitterRange - latencyJitterRange/2))
		jitterJ := latencyJ.MedianMs * (1.0 + (jitterFn()*latencyJitterRange - latencyJitterRange/2))

		return jitterI < jitterJ
	})

	return indices
}
