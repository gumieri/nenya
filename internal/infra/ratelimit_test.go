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
		{"tpm block mid-size", 0, 100, 60, false, 2},
		{"tpm oversize admitted (NENYA-70)", 0, 100, 200, true, 1},
		{"both allow", 10, 10000, 500, true, 1},
		{"rpm blocks before tpm", 1, 10000, 500, false, 2},
		{"disabled", 0, 0, 100000, true, 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rl := NewRateLimiter(tt.maxRPM, tt.maxTPM)

			for i := 0; i < tt.requests; i++ {
				rl.Check("", "http://example.com/api", tt.tokens)
			}

			lastAllowed := rl.Check("", "http://example.com/api", tt.tokens)
			if lastAllowed != tt.wantAllow {
				t.Errorf("last request: expected allow=%v, got %v", tt.wantAllow, lastAllowed)
			}
		})
	}
}

func TestCheckRateLimitPerHost(t *testing.T) {
	rl := NewRateLimiter(10, 0)

	for i := 0; i < 10; i++ {
		if !rl.Check("", "http://host-a.example.com/api", 0) {
			t.Errorf("request %d to host-a should be allowed", i+1)
		}
	}
	for i := 0; i < 10; i++ {
		if !rl.Check("", "http://host-b.example.com/api", 0) {
			t.Errorf("request %d to host-b should be allowed", i+1)
		}
	}
	if rl.Check("", "http://host-a.example.com/api", 0) {
		t.Error("request 11 to host-a should be blocked")
	}
}

func TestCheckRateLimitURLParsing(t *testing.T) {
	rl := NewRateLimiter(10, 0)

	for i := 0; i < 10; i++ {
		if !rl.Check("", "http://example.com:8080/v1/chat/completions", 0) {
			t.Errorf("request %d should be allowed", i+1)
		}
	}
	rl.Check("", "http://example.com:8080/different/path", 0)
	if rl.Check("", "http://example.com:8080/v1/chat/completions", 0) {
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
			rl.Check("", "http://example.com/api", 0)
		}()
	}
	wg.Wait()
}

func TestCheckRateLimitRefill(t *testing.T) {
	rl := NewRateLimiter(2, 0)

	rl.Check("", "http://example.com/api", 0)
	rl.Check("", "http://example.com/api", 0)

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

	if !rl.Check("", "http://example.com/api", 0) {
		t.Error("after 60s refill, request should be allowed")
	}
}

func TestCheckRateLimitTPMAccumulation(t *testing.T) {
	rl := NewRateLimiter(0, 100)

	if !rl.Check("", "http://example.com/api", 60) {
		t.Error("first 60 tokens should be allowed")
	}
	if !rl.Check("", "http://example.com/api", 30) {
		t.Error("remaining 40 tokens should cover 30")
	}
	if rl.Check("", "http://example.com/api", 20) {
		t.Error("should be blocked: only ~10 tokens left, need 20")
	}
}

func TestCheckRateLimitHostCapacityEviction(t *testing.T) {
	rl := NewRateLimiter(10, 0)

	for i := 0; i < maxRateLimitHosts; i++ {
		if !rl.Check("", "http://host-"+strconv.Itoa(i)+".example.com/api", 0) {
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

	if !rl.Check("", "http://new-host.example.com/api", 0) {
		t.Error("new host should be allowed after stale entries evicted")
	}
}

func TestRateLimiterSnapshot(t *testing.T) {
	rl := NewRateLimiter(10, 100)
	rl.Check("", "http://host1.com/api", 50)
	rl.Check("", "http://host2.com/api", 30)

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

	if !rl.Check("", "http://fast.example.com/api", 0) {
		t.Fatal("first request should be allowed with global limits")
	}

	rl.SetProviderLimits("fast.example.com", ProviderRateLimits{MaxRPM: 500, MaxTPM: 200000})

	if !rl.Check("", "http://fast.example.com/api", 0) {
		t.Fatal("request should still be allowed after limit upgrade")
	}
}

func TestSetProviderLimits_NewHostUsesGlobalThenUpgraded(t *testing.T) {
	rl := NewRateLimiter(2, 0)

	rl.Check("", "http://limited.example.com/api", 0)
	rl.Check("", "http://limited.example.com/api", 0)

	if rl.Check("", "http://limited.example.com/api", 0) {
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

	if !rl.Check("", "http://limited.example.com/api", 0) {
		t.Fatal("request should be allowed after refill with upgraded RPM=100")
	}
}

func TestSetProviderLimits_ZeroDisablesDimension(t *testing.T) {
	rl := NewRateLimiter(15, 5000)

	rl.SetProviderLimits("unlimited.example.com", ProviderRateLimits{MaxRPM: 0, MaxTPM: 0})

	rl.Check("", "http://unlimited.example.com/api", 0)

	rl.mu.Lock()
	bucket := rl.limits["unlimited.example.com"]
	rl.mu.Unlock()

	bucket.mu.Lock()
	gotRPM := bucket.maxRPM
	gotTPM := bucket.maxTPM
	bucket.mu.Unlock()

	if gotRPM != 0 {
		t.Errorf("expected maxRPM disabled (0), got %d", gotRPM)
	}
	if gotTPM != 0 {
		t.Errorf("expected maxTPM disabled (0), got %d", gotTPM)
	}

	// Both dimensions disabled: requests must never be rejected, no matter
	// how many are issued or how large their token estimates are.
	for i := 0; i < 100; i++ {
		if !rl.Check("", "http://unlimited.example.com/api", 10_000_000) {
			t.Fatalf("request %d rejected despite disabled dimensions", i)
		}
	}
}

func TestSetProviderLimits_PartialDisableKeepsOtherDimension(t *testing.T) {
	rl := NewRateLimiter(0, 5000)

	// RPM disabled explicitly, TPM keeps the global default.
	rl.SetProviderLimits("mixed.example.com", ProviderRateLimits{MaxRPM: 0, MaxTPM: 100})

	rl.Check("", "http://mixed.example.com/api", 0)

	rl.mu.Lock()
	bucket := rl.limits["mixed.example.com"]
	rl.mu.Unlock()

	bucket.mu.Lock()
	gotRPM := bucket.maxRPM
	gotTPM := bucket.maxTPM
	bucket.mu.Unlock()

	if gotRPM != 0 {
		t.Errorf("expected maxRPM disabled (0), got %d", gotRPM)
	}
	if gotTPM != 100 {
		t.Errorf("expected maxTPM 100, got %d", gotTPM)
	}
}

func TestSetProviderLimits_NegativeValues(t *testing.T) {
	rl := NewRateLimiter(15, 5000)

	rl.SetProviderLimits("negative.example.com", ProviderRateLimits{MaxRPM: -5, MaxTPM: -100})

	rl.Check("", "http://negative.example.com/api", 0)

	rl.mu.Lock()
	bucket := rl.limits["negative.example.com"]
	rl.mu.Unlock()

	bucket.mu.Lock()
	gotRPM := bucket.maxRPM
	gotTPM := bucket.maxTPM
	bucket.mu.Unlock()

	// Negative values are clamped to 0, which disables the dimension.
	if gotRPM != 0 {
		t.Errorf("expected maxRPM clamped to disabled (0), got %d", gotRPM)
	}
	if gotTPM != 0 {
		t.Errorf("expected maxTPM clamped to disabled (0), got %d", gotTPM)
	}
}

func TestPerProviderIsolation(t *testing.T) {
	rl := NewRateLimiter(2, 0)

	rl.SetProviderLimits("provider-a.example.com", ProviderRateLimits{MaxRPM: 2, MaxTPM: 0})
	rl.SetProviderLimits("provider-b.example.com", ProviderRateLimits{MaxRPM: 100, MaxTPM: 0})

	rl.Check("", "http://provider-a.example.com/api", 0)
	rl.Check("", "http://provider-a.example.com/api", 0)

	if rl.Check("", "http://provider-a.example.com/api", 0) {
		t.Fatal("provider-a (RPM=2) should be blocked after 2 requests")
	}

	for i := 0; i < 50; i++ {
		if !rl.Check("", "http://provider-b.example.com/api", 0) {
			t.Fatalf("provider-b (RPM=100) request %d should be allowed", i+1)
		}
	}
}

func TestSetProviderLimits_SameHostProvidersIndependent(t *testing.T) {
	rl := NewRateLimiter(10, 1000)
	rl.SetProviderLimits("zai", ProviderRateLimits{MaxRPM: 2, MaxTPM: 1000})
	rl.SetProviderLimits("zai-coding-plan", ProviderRateLimits{MaxRPM: 60, MaxTPM: 50000})

	same := "https://api.z.ai/v1/chat/completions"

	// "zai" bucket: 2 RPM — third call must be rejected.
	if !rl.Check("zai", same, 0) {
		t.Fatal("zai should allow its first RPM")
	}
	if !rl.Check("zai", same, 0) {
		t.Fatal("zai should allow its second RPM")
	}
	if rl.Check("zai", same, 0) {
		t.Fatal("zai should reject the third call in the minute")
	}

	// "zai-coding-plan" shares the host but must have its own bucket.
	for range 60 {
		if !rl.Check("zai-coding-plan", same, 0) {
			t.Fatal("zai-coding-plan should not be affected by zai's bucket")
		}
	}
	if rl.Check("zai-coding-plan", same, 0) {
		t.Fatal("zai-coding-plan should eventually exhaust its own 60 RPM")
	}
}

func TestCheck_OversizedRequestAdmittedAndDrains(t *testing.T) {
	rl := NewRateLimiter(0, 100) // TPM-only: 100 tokens/min

	// A 500-token request exceeds the entire TPM capacity. It must be
	// admitted (NENYA-70) — the bucket can never refill to 500, so the old
	// reject-here behavior starved big requests permanently — and drain the
	// bucket to zero.
	if !rl.Check("p", "http://example.com/api", 500) {
		t.Fatal("oversized request should be admitted")
	}

	// The bucket is drained: a mid-size request now throttles until refill.
	allowed, rej := rl.CheckDetailed("p", "http://example.com/api", 50)
	if allowed {
		t.Fatal("mid-size request should throttle after the bucket was drained")
	}
	if rej.Dimension != RateLimitDimensionTPM {
		t.Fatalf("dimension = %q, want tpm", rej.Dimension)
	}
}

func TestCheck_MidSizeRejectionUnchanged(t *testing.T) {
	rl := NewRateLimiter(0, 100)
	if !rl.Check("p", "http://example.com/api", 60) {
		t.Fatal("first request should pass")
	}
	allowed, rej := rl.CheckDetailed("p", "http://example.com/api", 60)
	if allowed {
		t.Fatal("second request should throttle (40 tokens left < 60)")
	}
	if rej.Dimension != RateLimitDimensionTPM || rej.Limit != 100 || rej.TokenCount != 60 {
		t.Fatalf("rejection details = %+v", rej)
	}
	if rej.BucketLeft < 0 || rej.BucketLeft >= 60 {
		t.Fatalf("bucket_left = %v, want in [0,60)", rej.BucketLeft)
	}
}

func TestCheck_OversizedStillBoundByRPM(t *testing.T) {
	rl := NewRateLimiter(1, 100) // RPM 1, TPM 100

	if !rl.Check("p", "http://example.com/api", 500) {
		t.Fatal("first oversized request should be admitted")
	}
	allowed, rej := rl.CheckDetailed("p", "http://example.com/api", 500)
	if allowed {
		t.Fatal("second request should be RPM-throttled")
	}
	if rej.Dimension != RateLimitDimensionRPM {
		t.Fatalf("dimension = %q, want rpm (oversize bypass is TPM-only)", rej.Dimension)
	}
}

// TestRateLimiter_ChargeTPM pins the ChargeTPM accounting (NENYA-336): the
// charge credits BOTH time-based dimensions over the elapsed interval (the
// refill anchor is shared with Check — discarding the RPM accrual would
// starve the provider bucket the chat path shares), debits only the TPM
// dimension, and is a no-op for non-positive counts and nil limiters. The
// sub-cap setup makes a double-refill regression observable: the duplicated
// TPM accrual this test was written against produced 50, not 30.
func TestRateLimiter_ChargeTPM(t *testing.T) {
	rl := NewRateLimiter(60, 60)

	// Create the provider bucket with a full Check, then force it into a
	// sub-cap state with a known stale anchor so the refill amounts are
	// exact: 30s of elapsed at 60/min credits +30 to each dimension.
	if !rl.Check("p", "http://example.com/api", 0) {
		t.Fatal("first Check must be admitted (full bucket)")
	}
	var staleAnchor time.Time
	rl.mu.Lock()
	for _, b := range rl.limits {
		b.mu.Lock()
		b.rpmBucket = 10
		b.tpmBucket = 10
		b.lastRefill = time.Now().Add(-30 * time.Second)
		staleAnchor = b.lastRefill
		b.mu.Unlock()
	}
	rl.mu.Unlock()

	rl.ChargeTPM("p", "http://example.com/api", 10)

	rl.mu.Lock()
	b := rl.limits["p"]
	if b == nil {
		t.Fatal("provider bucket vanished")
	}
	rl.mu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	// The charge itself sets lastRefill = call time; the interval it
	// credited is exactly lastRefill − staleAnchor. Deriving the expectation
	// from the measured interval (not a fixed 30s) keeps the assertions
	// exact regardless of test-runner scheduling.
	elapsed := b.lastRefill.Sub(staleAnchor).Seconds()
	wantRPM := min(60.0, 10+elapsed*1.0)      // 60 RPM = 1 token/s
	wantTPM := min(60.0, 10+elapsed*1.0) - 10 // one 10-token debit
	// RPM: 10 + accrued (must NOT be discarded by the charge).
	if b.rpmBucket < wantRPM-1e-6 || b.rpmBucket > wantRPM+1e-6 {
		t.Errorf("rpmBucket = %v, want %v (elapsed %.3fs accrual credited)", b.rpmBucket, wantRPM, elapsed)
	}
	// TPM: 10 + accrued − 10 charged. The double-refill regression hit the
	// cap and produced 50.
	if b.tpmBucket < wantTPM-1e-6 || b.tpmBucket > wantTPM+1e-6 {
		t.Errorf("tpmBucket = %v, want %v (one refill credit, one 10-token debit)", b.tpmBucket, wantTPM)
	}
}

// TestRateLimiter_ChargeTPM_NoOps pins the guards: a nil limiter and
// non-positive token counts must not touch bucket state.
func TestRateLimiter_ChargeTPM_NoOps(t *testing.T) {
	var nilRL *RateLimiter
	nilRL.ChargeTPM("p", "http://example.com/api", 10) // must not panic

	rl := NewRateLimiter(60, 60)
	if !rl.Check("p", "http://example.com/api", 0) {
		t.Fatal("first Check must be admitted")
	}
	rl.mu.Lock()
	b := rl.limits["p"]
	rl.mu.Unlock()
	b.mu.Lock()
	snapshotRPM, snapshotTPM := b.rpmBucket, b.tpmBucket
	b.mu.Unlock()

	rl.ChargeTPM("p", "http://example.com/api", 0)
	rl.ChargeTPM("p", "http://example.com/api", -5)

	rl.mu.Lock()
	b = rl.limits["p"]
	rl.mu.Unlock()
	b.mu.Lock()
	rpmAfter, tpmAfter := b.rpmBucket, b.tpmBucket
	b.mu.Unlock()
	if rpmAfter != snapshotRPM || tpmAfter != snapshotTPM {
		t.Errorf("non-positive charge mutated state: rpm %v→%v, tpm %v→%v",
			snapshotRPM, rpmAfter, snapshotTPM, tpmAfter)
	}
}

// TestRateLimiter_CheckDetailed_RejectionAdvancesAnchor pins the anchor
// discipline: a REJECTED check must still advance lastRefill (the refill
// credits were already absorbed into the buckets). Without this, the next
// check re-credits the same elapsed interval — inflating the bucket during
// 429-retry storms and double-crediting ChargeTPM interleavings.
func TestRateLimiter_CheckDetailed_RejectionAdvancesAnchor(t *testing.T) {
	rl := NewRateLimiter(0, 100) // TPM-only: the RPM rejection path stays out of the way

	if !rl.Check("p", "http://example.com/api", 0) {
		t.Fatal("first Check must be admitted")
	}
	var staleAnchor time.Time
	rl.mu.Lock()
	for _, b := range rl.limits {
		b.mu.Lock()
		b.tpmBucket = 5
		b.lastRefill = time.Now().Add(-6 * time.Second)
		staleAnchor = b.lastRefill
		b.mu.Unlock()
	}
	rl.mu.Unlock()

	// 6s of elapsed at 100/min credits +10 → bucket 15; a 50-token request
	// exceeds the remaining budget (and is under the 100 cap) → rejected.
	allowed, rej := rl.CheckDetailed("p", "http://example.com/api", 50)
	if allowed || rej.Dimension != RateLimitDimensionTPM {
		t.Fatalf("expected a TPM rejection, got allowed=%v dimension=%q", allowed, rej.Dimension)
	}

	// The anchor advanced with the rejection: an immediate retry credits
	// ≈0 elapsed and must stay rejected with an unchanged bucket.
	rl.mu.Lock()
	b := rl.limits["p"]
	rl.mu.Unlock()
	b.mu.Lock()
	anchor := b.lastRefill
	bucketAfterFirst := b.tpmBucket
	b.mu.Unlock()
	if time.Since(anchor) > 2*time.Second {
		t.Fatalf("lastRefill = %v, want ≈now (the rejection must absorb the interval)", anchor)
	}

	// Pin the refill MAGNITUDE: the credited interval is anchor − staleAnchor,
	// so the bucket must equal min(cap, 5 + elapsed·100/60) — a duplicated
	// refill block would produce ≈25 and fail this exact comparison.
	elapsed := anchor.Sub(staleAnchor).Seconds()
	wantBucket := min(100.0, 5+elapsed*100.0/60.0)
	if bucketAfterFirst < wantBucket-1e-6 || bucketAfterFirst > wantBucket+1e-6 {
		t.Errorf("tpmBucket = %v, want %v (one 6s accrual over the measured interval)", bucketAfterFirst, wantBucket)
	}

	allowed2, _ := rl.CheckDetailed("p", "http://example.com/api", 50)
	if allowed2 {
		t.Fatal("immediate retry was admitted: the rejection re-credited the same elapsed interval (anchor did not advance)")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// The immediate retry may credit only the microseconds between calls
	// (~1e-6 tokens) — what it must NOT do is re-credit the full 6s
	// interval (+10, which would flip it to admitted).
	// +0.5 tolerates ≈18s of scheduler accrual (100 TPM ≈ 1.67 tok/s) while
	// staying 20× below the +10 full-interval re-credit this guards against.
	if got := b.tpmBucket; got >= bucketAfterFirst+0.5 {
		t.Errorf("tpmBucket = %v after the retry, want ≈%v (no full-interval re-credit)", got, bucketAfterFirst)
	}
}
