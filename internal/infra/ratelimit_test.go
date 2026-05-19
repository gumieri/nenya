package infra

import (
	"strconv"
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
			rl := NewRateLimiter(tt.maxRPM, tt.maxTPM)

			for i := 0; i < tt.requests; i++ {
				rl.Check("http://example.com/api", tt.tokens)
			}

			lastAllowed := rl.Check("http://example.com/api", tt.tokens)
			if lastAllowed != tt.wantAllow {
				t.Errorf("last request: expected allow=%v, got %v", tt.wantAllow, lastAllowed)
			}
		})
	}
}

func TestCheckRateLimitPerHost(t *testing.T) {
	rl := NewRateLimiter(10, 0)

	for i := 0; i < 10; i++ {
		if !rl.Check("http://host-a.example.com/api", 0) {
			t.Errorf("request %d to host-a should be allowed", i+1)
		}
	}
	for i := 0; i < 10; i++ {
		if !rl.Check("http://host-b.example.com/api", 0) {
			t.Errorf("request %d to host-b should be allowed", i+1)
		}
	}
	if rl.Check("http://host-a.example.com/api", 0) {
		t.Error("request 11 to host-a should be blocked")
	}
}

func TestCheckRateLimitURLParsing(t *testing.T) {
	rl := NewRateLimiter(10, 0)

	for i := 0; i < 10; i++ {
		if !rl.Check("http://example.com:8080/v1/chat/completions", 0) {
			t.Errorf("request %d should be allowed", i+1)
		}
	}
	rl.Check("http://example.com:8080/different/path", 0)
	if rl.Check("http://example.com:8080/v1/chat/completions", 0) {
		t.Error("request after bucket exhausted should be blocked")
	}
}

func TestCheckRateLimitConcurrent(t *testing.T) {
	rl := NewRateLimiter(1000, 0)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rl.Check("http://example.com/api", 0)
		}()
	}
	wg.Wait()
}

func TestCheckRateLimitRefill(t *testing.T) {
	rl := NewRateLimiter(2, 0)

	rl.Check("http://example.com/api", 0)
	rl.Check("http://example.com/api", 0)

	snapshot := rl.Snapshot()
	limiter, ok := snapshot["example.com"]
	if !ok {
		t.Fatal("expected rate limiter for example.com")
	}
	if limiter.RPM >= 1.0 {
		t.Errorf("expected RPM bucket depleted, got %f", limiter.RPM)
	}

	// Manually age the bucket
	rl.mu.Lock()
	hostLimiter, exists := rl.limits["example.com"]
	rl.mu.Unlock()
	if !exists {
		t.Fatal("expected internal limiter")
	}
	hostLimiter.mu.Lock()
	hostLimiter.lastRefill = time.Now().Add(-60 * time.Second)
	hostLimiter.mu.Unlock()

	if !rl.Check("http://example.com/api", 0) {
		t.Error("after 60s refill, request should be allowed")
	}
}

func TestCheckRateLimitTPMAccumulation(t *testing.T) {
	rl := NewRateLimiter(0, 100)

	if !rl.Check("http://example.com/api", 60) {
		t.Error("first 60 tokens should be allowed")
	}
	if !rl.Check("http://example.com/api", 30) {
		t.Error("remaining 40 tokens should cover 30")
	}
	if rl.Check("http://example.com/api", 20) {
		t.Error("should be blocked: only ~10 tokens left, need 20")
	}
}

func TestCheckRateLimitHostCapacityEviction(t *testing.T) {
	rl := NewRateLimiter(10, 0)

	for i := 0; i < maxRateLimitHosts; i++ {
		if !rl.Check("http://host-"+strconv.Itoa(i)+".example.com/api", 0) {
			t.Errorf("host %d should be allowed", i)
		}
	}

	rl.mu.Lock()
	for _, l := range rl.limits {
		l.mu.Lock()
		l.lastRefill = time.Now().Add(-staleHostThreshold)
		l.mu.Unlock()
	}
	rl.mu.Unlock()

	if !rl.Check("http://new-host.example.com/api", 0) {
		t.Error("new host should be allowed after stale entries evicted")
	}
}

func TestRateLimiterSnapshot(t *testing.T) {
	rl := NewRateLimiter(10, 100)
	rl.Check("http://host1.com/api", 50)
	rl.Check("http://host2.com/api", 30)

	snapshot := rl.Snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("expected 2 hosts, got %d", len(snapshot))
	}
	s1, ok := snapshot["host1.com"]
	if !ok {
		t.Fatal("expected host1.com")
	}
	if s1.RPM >= 10 || s1.RPM < 8 {
		t.Errorf("host1 RPM out of range: %f", s1.RPM)
	}
}

func TestSetProviderLimits_OverridesBucket(t *testing.T) {
	rl := NewRateLimiter(10, 1000)

	if !rl.Check("http://fast.example.com/api", 0) {
		t.Fatal("first request should be allowed with global limits")
	}

	rl.SetProviderLimits("fast.example.com", ProviderRateLimits{MaxRPM: 500, MaxTPM: 200000})

	if !rl.Check("http://fast.example.com/api", 0) {
		t.Fatal("request should still be allowed after limit upgrade")
	}
}

func TestSetProviderLimits_NewHostUsesGlobalThenUpgraded(t *testing.T) {
	rl := NewRateLimiter(2, 0)

	rl.Check("http://limited.example.com/api", 0)
	rl.Check("http://limited.example.com/api", 0)

	if rl.Check("http://limited.example.com/api", 0) {
		t.Fatal("third request should be blocked with global RPM=2")
	}

	rl.SetProviderLimits("limited.example.com", ProviderRateLimits{MaxRPM: 100, MaxTPM: 0})

	rl.mu.Lock()
	bucket := rl.limits["limited.example.com"]
	rl.mu.Unlock()

	bucket.mu.Lock()
	bucket.lastRefill = time.Now().Add(-60 * time.Second)
	bucket.rpmBucket = 0
	bucket.mu.Unlock()

	if !rl.Check("http://limited.example.com/api", 0) {
		t.Fatal("request should be allowed after refill with upgraded RPM=100")
	}
}

func TestSetProviderLimits_ZeroFallsBackToGlobal(t *testing.T) {
	rl := NewRateLimiter(15, 5000)

	rl.SetProviderLimits("fallback.example.com", ProviderRateLimits{MaxRPM: 0, MaxTPM: 0})

	rl.Check("http://fallback.example.com/api", 0)

	rl.mu.Lock()
	bucket := rl.limits["fallback.example.com"]
	rl.mu.Unlock()

	bucket.mu.Lock()
	gotRPM := bucket.maxRPM
	gotTPM := bucket.maxTPM
	bucket.mu.Unlock()

	if gotRPM != 15 {
		t.Errorf("expected maxRPM to fall back to global 15, got %d", gotRPM)
	}
	if gotTPM != 5000 {
		t.Errorf("expected maxTPM to fall back to global 5000, got %d", gotTPM)
	}
}

func TestSetProviderLimits_NegativeValues(t *testing.T) {
	rl := NewRateLimiter(15, 5000)

	rl.SetProviderLimits("fallback.example.com", ProviderRateLimits{MaxRPM: -5, MaxTPM: -100})

	rl.Check("http://fallback.example.com/api", 0)

	rl.mu.Lock()
	bucket := rl.limits["fallback.example.com"]
	rl.mu.Unlock()

	bucket.mu.Lock()
	gotRPM := bucket.maxRPM
	gotTPM := bucket.maxTPM
	bucket.mu.Unlock()

	if gotRPM != 15 {
		t.Errorf("expected maxRPM to fall back to global 15, got %d", gotRPM)
	}
	if gotTPM != 5000 {
		t.Errorf("expected maxTPM to fall back to global 5000, got %d", gotTPM)
	}
}

func TestPerProviderIsolation(t *testing.T) {
	rl := NewRateLimiter(2, 0)

	rl.SetProviderLimits("provider-a.example.com", ProviderRateLimits{MaxRPM: 2, MaxTPM: 0})
	rl.SetProviderLimits("provider-b.example.com", ProviderRateLimits{MaxRPM: 100, MaxTPM: 0})

	rl.Check("http://provider-a.example.com/api", 0)
	rl.Check("http://provider-a.example.com/api", 0)

	if rl.Check("http://provider-a.example.com/api", 0) {
		t.Fatal("provider-a (RPM=2) should be blocked after 2 requests")
	}

	for i := 0; i < 50; i++ {
		if !rl.Check("http://provider-b.example.com/api", 0) {
			t.Fatalf("provider-b (RPM=100) request %d should be allowed", i+1)
		}
	}
}
