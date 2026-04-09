package infra

import (
	"net/url"
	"sync"
	"time"
)

type RateLimiter struct {
	mu         sync.Mutex
	limits     map[string]*rateLimiter
	maxRPM     int
	maxTPM     int
	maxHosts   int
}

type rateLimiter struct {
	mu         sync.Mutex
	rpmBucket  float64
	tpmBucket  float64
	lastRefill time.Time
}

const maxRateLimitHosts = 100

func NewRateLimiter(maxRPM, maxTPM int) *RateLimiter {
	return &RateLimiter{
		limits:   make(map[string]*rateLimiter),
		maxRPM:   maxRPM,
		maxTPM:   maxTPM,
		maxHosts: maxRateLimitHosts,
	}
}

func (rl *RateLimiter) Check(upstreamURL string, tokenCount int) bool {
	host := upstreamURL
	if u, err := url.Parse(upstreamURL); err == nil && u.Host != "" {
		host = u.Host
	}

	rl.mu.Lock()
	limiter, exists := rl.limits[host]
	if !exists {
		if len(rl.limits) >= rl.maxHosts {
			rl.evictLocked()
		}
		if len(rl.limits) >= rl.maxHosts {
			rl.mu.Unlock()
			return false
		}
		limiter = &rateLimiter{
			rpmBucket:  float64(rl.maxRPM),
			tpmBucket:  float64(rl.maxTPM),
			lastRefill: time.Now(),
		}
		rl.limits[host] = limiter
	}
	rl.mu.Unlock()

	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(limiter.lastRefill).Seconds()

	if rl.maxRPM > 0 {
		limiter.rpmBucket = min(float64(rl.maxRPM),
			limiter.rpmBucket+elapsed*float64(rl.maxRPM)/60.0)
	}
	if rl.maxTPM > 0 {
		limiter.tpmBucket = min(float64(rl.maxTPM),
			limiter.tpmBucket+elapsed*float64(rl.maxTPM)/60.0)
	}

	if rl.maxRPM > 0 && limiter.rpmBucket < 1.0 {
		return false
	}
	if rl.maxTPM > 0 && limiter.tpmBucket < float64(tokenCount) {
		return false
	}

	if rl.maxRPM > 0 {
		limiter.rpmBucket--
	}
	if rl.maxTPM > 0 {
		limiter.tpmBucket -= float64(tokenCount)
	}
	limiter.lastRefill = now
	return true
}

func (rl *RateLimiter) evictLocked() {
	now := time.Now()
	for host, l := range rl.limits {
		l.mu.Lock()
		stale := now.Sub(l.lastRefill) > 5*time.Minute
		l.mu.Unlock()
		if stale {
			delete(rl.limits, host)
		}
	}
}

func (rl *RateLimiter) Snapshot() map[string]*RateLimitSnapshot {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	result := make(map[string]*RateLimitSnapshot, len(rl.limits))
	for host, l := range rl.limits {
		l.mu.Lock()
		result[host] = &RateLimitSnapshot{RPM: l.rpmBucket, TPM: l.tpmBucket}
		l.mu.Unlock()
	}
	return result
}
