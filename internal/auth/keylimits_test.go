package auth

import (
	"sync"
	"testing"
	"time"
)

// TestKeyUsageTracker_RPMWindow verifies the fixed one-minute window: N
// requests pass, the N+1th is denied, and the window resets.
func TestKeyUsageTracker_RPMWindow(t *testing.T) {
	now := time.Now()
	tr := NewKeyUsageTracker(func() time.Time { return now })

	for i := 0; i < 3; i++ {
		if !tr.AllowRequest("k", 3) {
			t.Fatalf("request %d denied within window", i+1)
		}
	}
	if tr.AllowRequest("k", 3) {
		t.Fatal("request 4 should be denied at rpm=3")
	}

	now = now.Add(61 * time.Second)
	if !tr.AllowRequest("k", 3) {
		t.Fatal("window should reset after a minute")
	}
}

// TestKeyUsageTracker_RPMDisabled verifies 0 means unlimited.
func TestKeyUsageTracker_RPMDisabled(t *testing.T) {
	tr := NewKeyUsageTracker(nil)
	for i := 0; i < 1000; i++ {
		if !tr.AllowRequest("k", 0) {
			t.Fatal("unlimited key must never be denied")
		}
	}
}

// TestKeyUsageTracker_DailyBudget verifies daily charging and UTC-day
// rollover.
func TestKeyUsageTracker_DailyBudget(t *testing.T) {
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	tr := NewKeyUsageTracker(func() time.Time { return now })

	if !tr.ChargeKeyTokens("k", 1000, 600) {
		t.Fatal("first charge should fit")
	}
	if tr.ChargeKeyTokens("k", 1000, 500) {
		t.Fatal("charge exceeding budget must be denied")
	}
	// Denied charge must not consume budget: 400 still fits.
	if !tr.ChargeKeyTokens("k", 1000, 400) {
		t.Fatal("400 should fit in remaining 400")
	}
	if tr.ChargeKeyTokens("k", 1000, 1) {
		t.Fatal("budget exhausted")
	}

	now = now.Add(24 * time.Hour)
	if !tr.ChargeKeyTokens("k", 1000, 1000) {
		t.Fatal("new UTC day should reset the budget")
	}
}

// TestKeyUsageTracker_ProviderTiers verifies the NENYA-20 tier semantics:
// a provider budget denies "fill" keys once exhausted while "always" keys
// keep being served (charging into exhaustion).
func TestKeyUsageTracker_ProviderTiers(t *testing.T) {
	tr := NewKeyUsageTracker(nil)

	if !tr.AllowProviderTokens("prov", "fill", 1000, 800) {
		t.Fatal("fill within headroom should pass")
	}
	if tr.AllowProviderTokens("prov", "fill", 1000, 300) {
		t.Fatal("fill must be denied when exhausted")
	}
	if !tr.AllowProviderTokens("prov", "always", 1000, 5000) {
		t.Fatal("always tier must be served despite exhaustion")
	}
}

// TestKeyUsageTracker_Concurrent exercises concurrent access for the race
// detector (run with -race).
func TestKeyUsageTracker_Concurrent(t *testing.T) {
	tr := NewKeyUsageTracker(nil)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tr.AllowRequest("k", 100000)
			tr.ChargeKeyTokens("k", 1<<30, 1)
			tr.AllowProviderTokens("p", "fill", 1<<30, 1)
			tr.Snapshot()
		}(i)
	}
	wg.Wait()
}
