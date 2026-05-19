package infra

import (
	"net/url"
	"sync"
	"time"
)

// RateLimiter manages per-host rate limiting with provider-specific RPM/TPM limits.
// When Check is called for a new host, it uses the provided limits if available,
// otherwise falls back to the global defaults.
type RateLimiter struct {
	mu             sync.Mutex
	limits         map[string]*rateLimiter
	providerLimits map[string]ProviderRateLimits
	maxRPM         int
	maxTPM         int
	maxHosts       int
}

// rateLimiter tracks the token bucket state for a single host.
type rateLimiter struct {
	mu         sync.Mutex
	rpmBucket  float64
	tpmBucket  float64
	maxRPM     int
	maxTPM     int
	lastRefill time.Time
}

// ProviderRateLimits defines the maximum requests per minute (RPM) and tokens
// per minute (TPM) for a specific upstream provider. Used by
// RateLimiter.SetProviderLimits to override global governance defaults.
type ProviderRateLimits struct {
	MaxRPM int
	MaxTPM int
}

const (
	maxRateLimitHosts  = 100
	staleHostThreshold = 5 * time.Minute
)

// NewRateLimiter creates a RateLimiter with global default limits.
func NewRateLimiter(maxRPM, maxTPM int) *RateLimiter {
	return &RateLimiter{
		limits:         make(map[string]*rateLimiter),
		providerLimits: make(map[string]ProviderRateLimits),
		maxRPM:         maxRPM,
		maxTPM:         maxTPM,
		maxHosts:       maxRateLimitHosts,
	}
}

// Check tests whether a request to the given URL is allowed under rate limits.
// It uses the per-host bucket and applies either provider-specific limits (if set
// via SetProviderLimits) or the global defaults.
func (rl *RateLimiter) Check(upstreamURL string, tokenCount int) bool {
	host := upstreamURL
	if u, err := url.Parse(upstreamURL); err == nil && u.Host != "" {
		host = u.Host
	}

	limiter := rl.getOrCreateBucket(host)
	if limiter == nil {
		return false
	}

	return limiter.check(tokenCount)
}

// getOrCreateBucket returns the rate limiter for the given host, creating one
// if it doesn't exist. Returns nil if the host capacity is exhausted.
func (rl *RateLimiter) getOrCreateBucket(host string) *rateLimiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	limiter, exists := rl.limits[host]
	if exists {
		return limiter
	}

	if len(rl.limits) >= rl.maxHosts {
		rl.evictLocked()
	}
	if len(rl.limits) >= rl.maxHosts {
		return nil
	}

	rpm, tpm := rl.effectiveLimits(host)
	limiter = &rateLimiter{
		rpmBucket:  float64(rpm),
		tpmBucket:  float64(tpm),
		maxRPM:     rpm,
		maxTPM:     tpm,
		lastRefill: time.Now(),
	}
	rl.limits[host] = limiter
	return limiter
}

// effectiveLimits returns the RPM/TPM for a host, using provider-specific
// limits if set, otherwise the global defaults.
func (rl *RateLimiter) effectiveLimits(host string) (int, int) {
	rpm, tpm := rl.maxRPM, rl.maxTPM
	if pl, ok := rl.providerLimits[host]; ok {
		if pl.MaxRPM > 0 {
			rpm = pl.MaxRPM
		}
		if pl.MaxTPM > 0 {
			tpm = pl.MaxTPM
		}
	}
	return rpm, tpm
}

// check tests whether a single request consuming tokenCount tokens is allowed.
func (l *rateLimiter) check(tokenCount int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(l.lastRefill).Seconds()

	if l.maxRPM > 0 {
		l.rpmBucket = min(float64(l.maxRPM),
			l.rpmBucket+elapsed*float64(l.maxRPM)/60.0)
	}
	if l.maxTPM > 0 {
		l.tpmBucket = min(float64(l.maxTPM),
			l.tpmBucket+elapsed*float64(l.maxTPM)/60.0)
	}

	if l.maxRPM > 0 && l.rpmBucket < 1.0 {
		return false
	}
	if l.maxTPM > 0 && l.tpmBucket < float64(tokenCount) {
		return false
	}

	if l.maxRPM > 0 {
		l.rpmBucket--
	}
	if l.maxTPM > 0 {
		l.tpmBucket -= float64(tokenCount)
	}
	l.lastRefill = now
	return true
}

// SetProviderLimits updates the rate limits for a specific host.
// If a bucket already exists for the host, its limits are updated immediately.
// Zero or negative values fall back to the global defaults. To disable rate
// limiting for a provider, set the global governance limits to zero instead.
//
// Lock ordering: rl.mu (global) → limiter.mu (per-bucket). This order must
// never be inverted elsewhere in the codebase.
func (rl *RateLimiter) SetProviderLimits(host string, limits ProviderRateLimits) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if limits.MaxRPM <= 0 {
		limits.MaxRPM = rl.maxRPM
	}
	if limits.MaxTPM <= 0 {
		limits.MaxTPM = rl.maxTPM
	}

	rl.providerLimits[host] = limits

	if limiter, exists := rl.limits[host]; exists {
		limiter.mu.Lock()
		limiter.maxRPM = limits.MaxRPM
		limiter.maxTPM = limits.MaxTPM
		limiter.mu.Unlock()
	}
}

// evictLocked removes stale rate limiter entries (not accessed in 5 minutes).
// Caller must hold rl.mu.
func (rl *RateLimiter) evictLocked() {
	now := time.Now()
	for host, l := range rl.limits {
		l.mu.Lock()
		stale := now.Sub(l.lastRefill) > staleHostThreshold
		l.mu.Unlock()
		if stale {
			delete(rl.limits, host)
		}
	}
}

// Snapshot returns a read-only view of all rate limit buckets.
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
