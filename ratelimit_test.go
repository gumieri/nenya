package main

import (
	"log/slog"
	"sync"
	"testing"
	"time"
)

func TestCheckRateLimit(t *testing.T) {
	tests := []struct {
		name      string
		maxRPM    int
		maxTPM    int
		tokens    int
		wantAllow bool
		requests  int
	}{
		{"rpm allow", 10, 0, 0, true, 1},
		{"rpm block", 2, 0, 0, false, 3},
		{"tpm allow", 0, 1000, 500, true, 1},
		{"tpm block", 0, 100, 200, false, 1},
		{"both allow", 10, 10000, 500, true, 1},
		{"rpm blocks before tpm", 1, 10000, 500, false, 2},
		{"disabled", 0, 0, 100000, true, 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				RateLimit: RateLimitConfig{MaxRPM: tt.maxRPM, MaxTPM: tt.maxTPM},
			}
			g := NewNenyaGateway(cfg, &SecretsConfig{}, slog.Default())

			allowed := 0
			for i := 0; i < tt.requests; i++ {
				if g.checkRateLimit("http://example.com/api", tt.tokens) {
					allowed++
				}
			}

			lastAllowed := g.checkRateLimit("http://example.com/api", tt.tokens)
			if lastAllowed != tt.wantAllow {
				t.Errorf("last request: expected allow=%v, got %v (allowed %d/%d)",
					tt.wantAllow, lastAllowed, allowed, tt.requests)
			}
		})
	}
}

func TestCheckRateLimitPerHost(t *testing.T) {
	cfg := Config{
		RateLimit: RateLimitConfig{MaxRPM: 10, MaxTPM: 0},
	}
	g := NewNenyaGateway(cfg, &SecretsConfig{}, slog.Default())

	for i := 0; i < 10; i++ {
		if !g.checkRateLimit("http://host-a.example.com/api", 0) {
			t.Errorf("request %d to host-a should be allowed", i+1)
		}
	}
	for i := 0; i < 10; i++ {
		if !g.checkRateLimit("http://host-b.example.com/api", 0) {
			t.Errorf("request %d to host-b should be allowed", i+1)
		}
	}
	if g.checkRateLimit("http://host-a.example.com/api", 0) {
		t.Error("request 11 to host-a should be blocked")
	}
}

func TestCheckRateLimitURLParsing(t *testing.T) {
	cfg := Config{
		RateLimit: RateLimitConfig{MaxRPM: 10, MaxTPM: 0},
	}
	g := NewNenyaGateway(cfg, &SecretsConfig{}, slog.Default())

	for i := 0; i < 10; i++ {
		if !g.checkRateLimit("http://example.com:8080/v1/chat/completions", 0) {
			t.Errorf("request %d should be allowed", i+1)
		}
	}
	g.checkRateLimit("http://example.com:8080/different/path", 0)
	if g.checkRateLimit("http://example.com:8080/v1/chat/completions", 0) {
		t.Error("request after bucket exhausted should be blocked")
	}
}

func TestCheckRateLimitConcurrent(t *testing.T) {
	cfg := Config{
		RateLimit: RateLimitConfig{MaxRPM: 1000, MaxTPM: 0},
	}
	g := NewNenyaGateway(cfg, &SecretsConfig{}, slog.Default())

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.checkRateLimit("http://example.com/api", 0)
		}()
	}
	wg.Wait()
}

func TestCheckRateLimitRefill(t *testing.T) {
	cfg := Config{
		RateLimit: RateLimitConfig{MaxRPM: 2, MaxTPM: 0},
	}
	g := NewNenyaGateway(cfg, &SecretsConfig{}, slog.Default())

	g.checkRateLimit("http://example.com/api", 0)
	g.checkRateLimit("http://example.com/api", 0)

	limiter, _ := g.rateLimits["example.com"]
	limiter.mu.Lock()
	limiter.lastRefill = time.Now().Add(-60 * time.Second)
	limiter.mu.Unlock()

	if !g.checkRateLimit("http://example.com/api", 0) {
		t.Error("after 60s refill, request should be allowed")
	}
}

func TestCheckRateLimitTPMAccumulation(t *testing.T) {
	cfg := Config{
		RateLimit: RateLimitConfig{MaxRPM: 0, MaxTPM: 100},
	}
	g := NewNenyaGateway(cfg, &SecretsConfig{}, slog.Default())

	if !g.checkRateLimit("http://example.com/api", 60) {
		t.Error("first 60 tokens should be allowed")
	}
	if !g.checkRateLimit("http://example.com/api", 30) {
		t.Error("remaining 40 tokens should cover 30")
	}
	if g.checkRateLimit("http://example.com/api", 20) {
		t.Error("should be blocked: only ~10 tokens left, need 20")
	}
}
