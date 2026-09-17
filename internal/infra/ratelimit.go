package infra

import (
	"net/url"
	"sync"
	"time"
)

// RateLimiter manages rate limiting with provider-specific RPM/TPM limits.
// Buckets are keyed by provider name when the caller supplies one, falling
// back to the upstream host for anonymous calls. Provider keying prevents
// same-host providers (e.g. "zai" and "zai-coding-plan", both api.z.ai)
// from silently sharing one bucket.
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

// RateLimitRejection describes why a request was rejected by the rate
// limiter. Dimension is empty when the request was allowed.
type RateLimitRejection struct {
	// Dimension is "rpm" or "tpm"; empty when the request was allowed.
	Dimension string
	// Limit is the configured limit of the rejecting dimension.
	Limit int
	// BucketLeft is the remaining budget (requests or tokens) at reject time.
	BucketLeft float64
	// TokenCount is the request's token estimate that was checked.
	TokenCount int
}

// Check tests whether a request to the given URL is allowed under rate limits.
// providerName scopes the bucket to the provider (preferred); when empty the
// bucket falls back to the upstream host. Provider-specific limits (set via
// SetProviderLimits) apply when set, otherwise the global defaults.
// For rejection details use CheckDetailed.
func (rl *RateLimiter) Check(providerName, upstreamURL string, tokenCount int) bool {
	allowed, _ := rl.CheckDetailed(providerName, upstreamURL, tokenCount)
	return allowed
}

// CheckDetailed is Check with rejection details: when allowed is false,
// the returned rejection names the exhausting dimension, its limit, the
// remaining bucket budget, and the request's token estimate.
func (rl *RateLimiter) CheckDetailed(providerName, upstreamURL string, tokenCount int) (bool, RateLimitRejection) {
	limiter := rl.getOrCreateBucket(bucketKey(providerName, upstreamURL))
	if limiter == nil {
		return false, RateLimitRejection{Dimension: "bucket_capacity", TokenCount: tokenCount}
	}

	return limiter.checkDetailed(tokenCount)
}

// checkDetailed is check with rejection details. It returns the rejection
// reason (dimension, limit, remaining budget) alongside the boolean verdict.
func (l *rateLimiter) checkDetailed(tokenCount int) (bool, RateLimitRejection) {
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
		return false, RateLimitRejection{
			Dimension: "rpm", Limit: l.maxRPM,
			BucketLeft: l.rpmBucket, TokenCount: tokenCount,
		}
	}
	if l.maxTPM > 0 && l.tpmBucket < float64(tokenCount) && float64(tokenCount) < float64(l.maxTPM) {
		return false, RateLimitRejection{
			Dimension: "tpm", Limit: l.maxTPM,
			BucketLeft: l.tpmBucket, TokenCount: tokenCount,
		}
	}

	if l.maxRPM > 0 {
		l.rpmBucket--
	}
	if l.maxTPM > 0 {
		l.tpmBucket = max(0, l.tpmBucket-float64(tokenCount))
	}
	l.lastRefill = now
	return true, RateLimitRejection{}
}

// bucketKey resolves the bucket key: provider name when set, otherwise the
// upstream URL's host, otherwise the raw URL.
func bucketKey(providerName, upstreamURL string) string {
	if providerName != "" {
		return providerName
	}
	if u, err := url.Parse(upstreamURL); err == nil && u.Host != "" {
		return u.Host
	}
	return upstreamURL
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

// SetProviderLimits updates the rate limits for a specific provider. The
// provider name doubles as the bucket key used by Check when callers pass
// the same name. If a bucket already exists, its limits are updated
// immediately. Zero or negative values fall back to the global defaults. To
// disable rate limiting for a provider, set the global governance limits to
// zero instead.
//
// Lock ordering: rl.mu (global) → limiter.mu (per-bucket). This order must
// never be inverted elsewhere in the codebase.
func (rl *RateLimiter) SetProviderLimits(providerName string, limits ProviderRateLimits) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if limits.MaxRPM <= 0 {
		limits.MaxRPM = rl.maxRPM
	}
	if limits.MaxTPM <= 0 {
		limits.MaxTPM = rl.maxTPM
	}

	rl.providerLimits[providerName] = limits

	if limiter, exists := rl.limits[providerName]; exists {
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
