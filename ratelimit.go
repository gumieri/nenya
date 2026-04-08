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

const maxRateLimitHosts = 100

func (g *NenyaGateway) checkRateLimit(upstreamURL string, tokenCount int) bool {
	host := upstreamURL
	if u, err := url.Parse(upstreamURL); err == nil && u.Host != "" {
		host = u.Host
	}

	g.rlMu.Lock()
	limiter, exists := g.rateLimits[host]
	if !exists {
		if len(g.rateLimits) >= maxRateLimitHosts {
			g.rateLimits = make(map[string]*rateLimiter)
		}
		limiter = &rateLimiter{
			rpmBucket:  float64(g.config.Governance.RatelimitMaxRPM),
			tpmBucket:  float64(g.config.Governance.RatelimitMaxTPM),
			lastRefill: time.Now(),
		}
		g.rateLimits[host] = limiter
	}
	g.rlMu.Unlock()

	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(limiter.lastRefill).Seconds()

	if g.config.Governance.RatelimitMaxRPM > 0 {
		limiter.rpmBucket = min(float64(g.config.Governance.RatelimitMaxRPM),
			limiter.rpmBucket+elapsed*float64(g.config.Governance.RatelimitMaxRPM)/60.0)
	}
	if g.config.Governance.RatelimitMaxTPM > 0 {
		limiter.tpmBucket = min(float64(g.config.Governance.RatelimitMaxTPM),
			limiter.tpmBucket+elapsed*float64(g.config.Governance.RatelimitMaxTPM)/60.0)
	}

	if g.config.Governance.RatelimitMaxRPM > 0 && limiter.rpmBucket < 1.0 {
		g.logger.Warn("RPM limit exceeded",
			"host", host, "rpm_available", limiter.rpmBucket)
		return false
	}
	if g.config.Governance.RatelimitMaxTPM > 0 && limiter.tpmBucket < float64(tokenCount) {
		g.logger.Warn("TPM limit exceeded",
			"host", host, "tpm_available", limiter.tpmBucket, "tokens_needed", tokenCount)
		return false
	}

	if g.config.Governance.RatelimitMaxRPM > 0 {
		limiter.rpmBucket--
	}
	if g.config.Governance.RatelimitMaxTPM > 0 {
		limiter.tpmBucket -= float64(tokenCount)
	}
	limiter.lastRefill = now
	return true
}
