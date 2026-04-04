package main

import (
	"net/url"
	"sync"
	"time"
)

type rateLimiter struct {
	mu         sync.Mutex
	rpmBucket  float64
	tpmBucket  float64
	lastRefill time.Time
}

func (g *NenyaGateway) checkRateLimit(upstreamURL string, tokenCount int) bool {
	host := upstreamURL
	if u, err := url.Parse(upstreamURL); err == nil && u.Host != "" {
		host = u.Host
	}

	g.rlMu.Lock()
	limiter, exists := g.rateLimits[host]
	if !exists {
		limiter = &rateLimiter{
			rpmBucket:  float64(g.config.RateLimit.MaxRPM),
			tpmBucket:  float64(g.config.RateLimit.MaxTPM),
			lastRefill: time.Now(),
		}
		g.rateLimits[host] = limiter
	}
	g.rlMu.Unlock()

	limiter.mu.Lock()
	defer limiter.mu.Unlock()

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
		g.logger.Warn("RPM limit exceeded",
			"host", host, "rpm_available", limiter.rpmBucket)
		return false
	}
	if g.config.RateLimit.MaxTPM > 0 && limiter.tpmBucket < float64(tokenCount) {
		g.logger.Warn("TPM limit exceeded",
			"host", host, "tpm_available", limiter.tpmBucket, "tokens_needed", tokenCount)
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
