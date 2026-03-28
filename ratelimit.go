package main

import (
	"log"
	"net/url"
	"time"
)

// rateLimiter is a token-bucket rate limiter for a single upstream host.
// Buckets refill continuously based on elapsed time, preventing the 2× burst
// that a fixed (tumbling) window allows at window boundaries.
// Protected by NenyaGateway.rlMu — no per-instance lock needed.
type rateLimiter struct {
	rpmBucket  float64   // available request tokens; capacity = MaxRPM
	tpmBucket  float64   // available payload tokens; capacity = MaxTPM
	lastRefill time.Time // when buckets were last topped up
}

// checkRateLimit verifies if the request is within RPM/TPM limits for the given upstream
// using a token-bucket algorithm. Buckets refill continuously so there is no burst at
// window boundaries. Returns true if the request is allowed (and consumes tokens).
func (g *NenyaGateway) checkRateLimit(upstreamURL string, tokenCount int) bool {
	host := upstreamURL
	if u, err := url.Parse(upstreamURL); err == nil && u.Host != "" {
		host = u.Host
	}

	g.rlMu.Lock()
	defer g.rlMu.Unlock()

	limiter, exists := g.rateLimits[host]
	if !exists {
		limiter = &rateLimiter{
			rpmBucket:  float64(g.config.RateLimit.MaxRPM),
			tpmBucket:  float64(g.config.RateLimit.MaxTPM),
			lastRefill: time.Now(),
		}
		g.rateLimits[host] = limiter
	}

	// Refill buckets proportional to elapsed time since the last call.
	now := time.Now()
	elapsed := now.Sub(limiter.lastRefill).Seconds()
	limiter.lastRefill = now

	if g.config.RateLimit.MaxRPM > 0 {
		limiter.rpmBucket = min(float64(g.config.RateLimit.MaxRPM),
			limiter.rpmBucket+elapsed*float64(g.config.RateLimit.MaxRPM)/60.0)
	}
	if g.config.RateLimit.MaxTPM > 0 {
		limiter.tpmBucket = min(float64(g.config.RateLimit.MaxTPM),
			limiter.tpmBucket+elapsed*float64(g.config.RateLimit.MaxTPM)/60.0)
	}

	if g.config.RateLimit.MaxRPM > 0 && limiter.rpmBucket < 1.0 {
		log.Printf("[RATELIMIT] RPM limit exceeded for %s (%.2f tokens available)", host, limiter.rpmBucket)
		return false
	}
	if g.config.RateLimit.MaxTPM > 0 && limiter.tpmBucket < float64(tokenCount) {
		log.Printf("[RATELIMIT] TPM limit exceeded for %s (%.0f available, %d needed)", host, limiter.tpmBucket, tokenCount)
		return false
	}

	if g.config.RateLimit.MaxRPM > 0 {
		limiter.rpmBucket--
	}
	if g.config.RateLimit.MaxTPM > 0 {
		limiter.tpmBucket -= float64(tokenCount)
	}
	return true
}
