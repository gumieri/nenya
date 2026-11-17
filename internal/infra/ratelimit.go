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

// Rate-limit rejection dimensions reported in RateLimitRejection.Dimension.
const (
	RateLimitDimensionRPM            = "rpm"
	RateLimitDimensionTPM            = "tpm"
	RateLimitDimensionBucketCapacity = "bucket_capacity"
)

// RateLimitRejection describes why a request was rejected by the rate
// limiter. Dimension is empty when the request was allowed.
type RateLimitRejection struct {
	// Dimension is the exhausting dimension: "rpm", "tpm", or
	// "bucket_capacity" (host capacity exhausted, no bucket exists); empty
	// when the request was allowed.
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
//
// Test-only convenience: production call sites must use CheckDetailed so
// rejection details (dimension/limit/remaining) reach the logs and the
// structured 429 body. Retained for test ergonomics — see issue #150's
// dead-code ledger before removing.
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
		return false, RateLimitRejection{Dimension: RateLimitDimensionBucketCapacity, TokenCount: tokenCount}
	}

	return limiter.checkDetailed(tokenCount)
}

// checkDetailed is check with rejection details. It returns the rejection
// reason (dimension, limit, remaining budget) alongside the boolean verdict.
func (l *rateLimiter) checkDetailed(tokenCount int) (bool, RateLimitRejection) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	// A backward clock step must not credit negative amounts: clamp the
	// elapsed interval to zero — the anchor still advances, so the skipped
	// interval is lost rather than double-counted (rate-limit conservatism).
	elapsed := max(0.0, now.Sub(l.lastRefill).Seconds())

	if l.maxRPM > 0 {
		l.rpmBucket = min(float64(l.maxRPM),
			l.rpmBucket+elapsed*float64(l.maxRPM)/60.0)
	}
	if l.maxTPM > 0 {
		l.tpmBucket = min(float64(l.maxTPM),
			l.tpmBucket+elapsed*float64(l.maxTPM)/60.0)
	}
	// The anchor advances on EVERY path once the credits are absorbed —
	// rejection paths included. Re-crediting the same elapsed interval on
	// the next call would inflate the buckets during 429-retry storms and
	// poison ChargeTPM interleavings.
	l.lastRefill = now

	if l.maxRPM > 0 && l.rpmBucket < 1.0 {
		return false, RateLimitRejection{
			Dimension: RateLimitDimensionRPM, Limit: l.maxRPM,
			BucketLeft: l.rpmBucket, TokenCount: tokenCount,
		}
	}
	if l.maxTPM > 0 && l.tpmBucket < float64(tokenCount) && float64(tokenCount) < float64(l.maxTPM) {
		return false, RateLimitRejection{
			Dimension: RateLimitDimensionTPM, Limit: l.maxTPM,
			BucketLeft: l.tpmBucket, TokenCount: tokenCount,
		}
	}

	if l.maxRPM > 0 {
		l.rpmBucket--
	}
	if l.maxTPM > 0 {
		l.tpmBucket = max(0, l.tpmBucket-float64(tokenCount))
	}
	return true, RateLimitRejection{}
}

// ChargeTPM decrements the TPM bucket by tokenCount without making an
// admission decision and without debiting RPM: the post-dispatch true-up
// for endpoints whose real token usage is only known after the response
// (NENYA-336 — embeddings). RPM accrual over the elapsed interval is still
// credited (the refill anchor is shared with Check); refill semantics
// mirror checkDetailed so a charge that lands after a refill boundary
// credits the elapsed budget first. tokenCount <= 0 is a no-op. The bucket
// is keyed exactly like Check, so a charge lands in the same provider
// bucket subsequent admission checks consult.
func (rl *RateLimiter) ChargeTPM(providerName, upstreamURL string, tokenCount int) {
	if rl == nil || tokenCount <= 0 {
		return
	}
	limiter := rl.getOrCreateBucket(bucketKey(providerName, upstreamURL))
	if limiter == nil {
		// Host capacity exhausted: the true-up is silently dropped — the
		// bucket it would land in no longer exists, so there is nothing to
		// charge. Accepted under-charge; Check callers hit the same
		// capacity wall first.
		return
	}
	limiter.chargeTPM(tokenCount)
}

// chargeTPM is ChargeTPM's bucket-level implementation. The refill credits
// BOTH time-based dimensions before the TPM debit: the lastRefill anchor is
// shared with Check, and discarding the RPM accrual over the interval would
// starve the bucket this provider shares with the chat path.
func (l *rateLimiter) chargeTPM(tokenCount int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	// Same backward-clock clamp as checkDetailed: credit nothing, lose the
	// interval, keep the anchor monotone.
	elapsed := max(0.0, now.Sub(l.lastRefill).Seconds())
	l.lastRefill = now

	if l.maxRPM > 0 {
		l.rpmBucket = min(float64(l.maxRPM),
			l.rpmBucket+elapsed*float64(l.maxRPM)/60.0)
	}
	if l.maxTPM > 0 {
		l.tpmBucket = min(float64(l.maxTPM),
			l.tpmBucket+elapsed*float64(l.maxTPM)/60.0)
		l.tpmBucket = max(0, l.tpmBucket-float64(tokenCount))
	}
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
// limits when set (verbatim: 0 disables the dimension for that provider),
// otherwise the global defaults.
func (rl *RateLimiter) effectiveLimits(host string) (int, int) {
	if pl, ok := rl.providerLimits[host]; ok {
		return max(0, pl.MaxRPM), max(0, pl.MaxTPM)
	}
	return rl.maxRPM, rl.maxTPM
}

// SetProviderLimits updates the rate limits for a specific provider. The
// provider name doubles as the bucket key used by Check when callers pass
// the same name. If a bucket already exists, its limits are updated
// immediately. Values are applied verbatim: 0 (or negative, clamped to 0)
// disables that dimension for the provider. Callers that want a dimension
// to inherit the global governance limit must resolve that themselves and
// pass the global value explicitly.
//
// Lock ordering: rl.mu (global) → limiter.mu (per-bucket). This order must
// never be inverted elsewhere in the codebase.
func (rl *RateLimiter) SetProviderLimits(providerName string, limits ProviderRateLimits) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	limits.MaxRPM = max(0, limits.MaxRPM)
	limits.MaxTPM = max(0, limits.MaxTPM)

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
