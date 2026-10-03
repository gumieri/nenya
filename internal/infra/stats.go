package infra

import (
	"sync"
	"sync/atomic"
	"time"
)

// TokenSnapshot holds per-request token usage statistics.
// It captures input, output, and total token counts for a single request.
type TokenSnapshot struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// modelStats holds the per-model counters aggregated by UsageTracker. The
// counters are serialized only via UsageTracker.Snapshot, which owns the JSON
// key names; the field names are package-private because the type is.
type modelStats struct {
	requests            uint64
	inputTokens         uint64
	outputTokens        uint64
	reasoningTokens     uint64
	cacheHitTokens      uint64
	cacheMissTokens     uint64
	cacheCreationTokens uint64
	errors              uint64
}

// UsageTracker aggregates token-usage counters per model for /statsz. Counter
// fields of each modelStats are updated with sync/atomic operations. The models
// map is read under mu.RLock (Snapshot) and inserted into under mu.Lock (the
// getOrCreate create path upgrades R->W and re-checks after the upgrade); start
// is written only during construction. A snapshot's counters are therefore
// eventually consistent rather than a single-instant view. Note that
// RecordRequest creates an entry even for malformed (negative) input, so a
// model may appear with only its request count set.
type UsageTracker struct {
	mu     sync.RWMutex
	models map[string]*modelStats
	start  time.Time
}

// NewUsageTracker returns an empty UsageTracker ready for use.
func NewUsageTracker() *UsageTracker {
	return &UsageTracker{
		models: make(map[string]*modelStats),
		start:  time.Now(),
	}
}

// getOrCreate returns the stats record for model, creating it on first use. A
// new record is published to the map only after it is fully constructed, so a
// concurrent RLock reader never observes a partially-initialized modelStats; the
// create path upgrades R->W and re-checks under the write lock so racing
// creators converge on one pointer.
func (u *UsageTracker) getOrCreate(model string) *modelStats {
	u.mu.RLock()
	s, ok := u.models[model]
	u.mu.RUnlock()
	if ok {
		return s
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok = u.models[model]
	if !ok {
		s = &modelStats{}
		u.models[model] = s
	}
	return s
}

// RecordRequest counts one request for model and adds inputTokens to its input
// total. input_tokens is the request-side count and is unrelated to
// cache_hit_ratio, whose numerator and denominator come from the cache_* token
// counters. A negative inputTokens is not added; the request is still counted,
// so requests may exceed the summed input_tokens.
func (u *UsageTracker) RecordRequest(model string, inputTokens int) {
	s := u.getOrCreate(model)
	// Malformed input must not drop the request: the guard sits after the
	// increment on purpose.
	atomic.AddUint64(&s.requests, 1)
	if inputTokens < 0 {
		return
	}
	atomic.AddUint64(&s.inputTokens, uint64(inputTokens))
}

// RecordOutput adds outputTokens to model's output total. Negative counts are
// ignored so a malformed usage object cannot wrap the unsigned counter.
func (u *UsageTracker) RecordOutput(model string, outputTokens int) {
	if outputTokens < 0 {
		return
	}
	s := u.getOrCreate(model)
	atomic.AddUint64(&s.outputTokens, uint64(outputTokens))
}

// RecordError counts one failed request for model.
func (u *UsageTracker) RecordError(model string) {
	s := u.getOrCreate(model)
	atomic.AddUint64(&s.errors, 1)
}

// RecordCacheHit adds upstream prompt-cache hit tokens (served from cache) to
// model's total. Negative counts are ignored.
func (u *UsageTracker) RecordCacheHit(model string, tokens int) {
	if tokens < 0 {
		return
	}
	s := u.getOrCreate(model)
	atomic.AddUint64(&s.cacheHitTokens, uint64(tokens))
}

// RecordCacheMiss adds upstream prompt-cache miss tokens (uncached input) to
// model's total. Negative counts are ignored.
func (u *UsageTracker) RecordCacheMiss(model string, tokens int) {
	if tokens < 0 {
		return
	}
	s := u.getOrCreate(model)
	atomic.AddUint64(&s.cacheMissTokens, uint64(tokens))
}

// RecordCacheCreation adds upstream prompt-cache creation tokens (newly written
// cache entries) to model's total. Negative counts are ignored.
func (u *UsageTracker) RecordCacheCreation(model string, tokens int) {
	if tokens < 0 {
		return
	}
	s := u.getOrCreate(model)
	atomic.AddUint64(&s.cacheCreationTokens, uint64(tokens))
}

// RecordReasoning adds reasoning tokens to model's total. Negative counts are
// ignored.
func (u *UsageTracker) RecordReasoning(model string, tokens int) {
	if tokens < 0 {
		return
	}
	s := u.getOrCreate(model)
	atomic.AddUint64(&s.reasoningTokens, uint64(tokens))
}

// Snapshot returns a JSON-serializable map of per-model usage counters plus the
// derived cache_hit_ratio (see cacheHitRatio for its formula and semantics).
// Counter fields are loaded independently, so a snapshot is eventually
// consistent rather than a single instant: a concurrent request may land
// between two loads. That is acceptable for an observability surface. The
// uint64 counters are exact in the JSON encoding; only cache_hit_ratio (a
// float64, alongside the integer counters — mixed numeric types for an
// in-process consumer) is approximate above 2^53, and a consumer that
// re-parses the tokens as float64 loses precision above 2^53 too.
func (u *UsageTracker) Snapshot() map[string]interface{} {
	u.mu.RLock()
	defer u.mu.RUnlock()

	modelsI := make(map[string]interface{}, len(u.models))
	for name, s := range u.models {
		hit := atomic.LoadUint64(&s.cacheHitTokens)
		miss := atomic.LoadUint64(&s.cacheMissTokens)
		creation := atomic.LoadUint64(&s.cacheCreationTokens)
		modelsI[name] = map[string]interface{}{
			"requests":              atomic.LoadUint64(&s.requests),
			"input_tokens":          atomic.LoadUint64(&s.inputTokens),
			"output_tokens":         atomic.LoadUint64(&s.outputTokens),
			"reasoning_tokens":      atomic.LoadUint64(&s.reasoningTokens),
			"cache_hit_tokens":      hit,
			"cache_miss_tokens":     miss,
			"cache_creation_tokens": creation,
			"cache_hit_ratio":       cacheHitRatio(hit, miss, creation),
			"errors":                atomic.LoadUint64(&s.errors),
		}
	}

	return map[string]interface{}{
		"uptime_seconds": int(time.Since(u.start).Seconds()),
		"models":         modelsI,
	}
}

// cacheHitRatio returns the fraction of accounted prompt-cache tokens served
// from cache for one model: hit / (hit + miss + creation). Creation tokens are
// newly written cache entries — a miss from the client's perspective — so they
// belong in the denominator. It returns 0 both for a model with no prompt-cache
// traffic and for a genuine 0% hit rate; the two are not distinguished.
// Precondition: only the normalized prompt_cache_hit_tokens /
// prompt_cache_miss_tokens / cache_creation_tokens feed this ratio. Whether the
// result is a true hit rate depends on the fields the provider path populates —
// OpenAI streaming supplies a hit with no miss/creation and therefore reads 1.0
// for any cached traffic, while OpenAI non-streaming reads the same
// prompt_tokens_details.cached_tokens fallback and also reads 1.0 for any
// cached traffic (0 only when the upstream reports neither a hit nor a miss).
//
// The sum is computed in float64 to avoid unsigned overflow; above 2^53
// accumulated tokens the numerator and denominator round independently and can
// collapse distinct ratios, so the result is approximate and not suitable for
// billing or exact accounting. The all-zero short-circuit is an integer
// comparison rather than a float sum, avoiding both NaN and an overflow-prone
// add.
func cacheHitRatio(hit, miss, creation uint64) float64 {
	if hit == 0 && miss == 0 && creation == 0 {
		return 0
	}
	hitF := float64(hit)
	return hitF / (hitF + float64(miss) + float64(creation))
}
