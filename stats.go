package main

import (
	"sync"
	"sync/atomic"
	"time"
)

type modelStats struct {
	Requests     uint64 `json:"requests"`
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
	Errors       uint64 `json:"errors"`
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

func (u *UsageTracker) RecordRequest(model string, inputTokens int) {
	s := u.getOrCreate(model)
	atomic.AddUint64(&s.Requests, 1)
	atomic.AddUint64(&s.InputTokens, uint64(inputTokens))
}

func (u *UsageTracker) RecordOutput(model string, outputTokens int) {
	s := u.getOrCreate(model)
	atomic.AddUint64(&s.OutputTokens, uint64(outputTokens))
}

func (u *UsageTracker) RecordError(model string) {
	s := u.getOrCreate(model)
	atomic.AddUint64(&s.Errors, 1)
}

func (u *UsageTracker) Snapshot() map[string]interface{} {
	u.mu.RLock()
	defer u.mu.RUnlock()

	models := make(map[string]modelStats, len(u.models))
	for name, s := range u.models {
		models[name] = modelStats{
			Requests:     atomic.LoadUint64(&s.Requests),
			InputTokens:  atomic.LoadUint64(&s.InputTokens),
			OutputTokens: atomic.LoadUint64(&s.OutputTokens),
			Errors:       atomic.LoadUint64(&s.Errors),
		}
	}

	return map[string]interface{}{
		"uptime_seconds": int(time.Since(u.start).Seconds()),
		"models":         models,
	}
}
