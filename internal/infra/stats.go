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

type modelStats struct {
	Requests        uint64 `json:"requests"`
	InputTokens     uint64 `json:"input_tokens"`
	OutputTokens    uint64 `json:"output_tokens"`
	CacheHitTokens  uint64 `json:"cache_hit_tokens"`
	CacheMissTokens uint64 `json:"cache_miss_tokens"`
	Errors          uint64 `json:"errors"`
}

type UsageTracker struct {
	mu     sync.RWMutex
	models map[string]*modelStats
	start  time.Time
}

func NewUsageTracker() *UsageTracker {
	return &UsageTracker{
		models: make(map[string]*modelStats),
		start:  time.Now(),
	}
}

func (u *UsageTracker) GetOrCreate(model string) *modelStats {
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

func (u *UsageTracker) RecordRequest(model string, inputTokens int) {
	s := u.GetOrCreate(model)
	atomic.AddUint64(&s.Requests, 1)
	atomic.AddUint64(&s.InputTokens, uint64(inputTokens))
}

func (u *UsageTracker) RecordOutput(model string, outputTokens int) {
	s := u.GetOrCreate(model)
	atomic.AddUint64(&s.OutputTokens, uint64(outputTokens))
}

func (u *UsageTracker) RecordError(model string) {
	s := u.GetOrCreate(model)
	atomic.AddUint64(&s.Errors, 1)
}

func (u *UsageTracker) RecordCacheHit(model string, tokens int) {
	s := u.GetOrCreate(model)
	atomic.AddUint64(&s.CacheHitTokens, uint64(tokens))
}

func (u *UsageTracker) RecordCacheMiss(model string, tokens int) {
	s := u.GetOrCreate(model)
	atomic.AddUint64(&s.CacheMissTokens, uint64(tokens))
}

func (u *UsageTracker) Snapshot() map[string]interface{} {
	u.mu.RLock()
	defer u.mu.RUnlock()

	modelsI := make(map[string]interface{}, len(u.models))
	for name, s := range u.models {
		modelsI[name] = map[string]interface{}{
			"requests":          atomic.LoadUint64(&s.Requests),
			"input_tokens":      atomic.LoadUint64(&s.InputTokens),
			"output_tokens":     atomic.LoadUint64(&s.OutputTokens),
			"cache_hit_tokens":  atomic.LoadUint64(&s.CacheHitTokens),
			"cache_miss_tokens": atomic.LoadUint64(&s.CacheMissTokens),
			"errors":            atomic.LoadUint64(&s.Errors),
		}
	}

	return map[string]interface{}{
		"uptime_seconds": int(time.Since(u.start).Seconds()),
		"models":         modelsI,
	}
}
