package resilience

import (
	"testing"
	"time"
)

// TestCircuitBreakerPerKeyThresholds pins NENYA-204: the per-key
// threshold lookup drives tripping and recovery — a key resolving to
// failure threshold 2 trips after 2 failures, a key inheriting the
// default (0) trips only at the constructor threshold, and the
// per-key success threshold governs HalfOpen recovery.
func TestCircuitBreakerPerKeyThresholds(t *testing.T) {
	cb := NewCircuitBreaker(5, 1, 1, time.Second, nil)
	cb.SetThresholdLookup(func(key string) (failure, success int) {
		if key == "fast:prov:model" {
			return 2, 2
		}
		return 0, 0 // inherit defaults (5, 1)
	})

	// fast trips at 2 failures.
	cb.RecordFailure("fast:prov:model")
	if !cb.Allow("fast:prov:model") {
		t.Fatal("fast tripped before its threshold")
	}
	cb.RecordFailure("fast:prov:model")
	if cb.Allow("fast:prov:model") {
		t.Fatal("fast did not trip at its per-key failure threshold of 2")
	}

	// slow (default 5) still allows after 2 and 4 failures.
	for i := 0; i < 4; i++ {
		cb.RecordFailure("slow:prov:model")
	}
	if !cb.Allow("slow:prov:model") {
		t.Fatal("slow tripped before the default failure threshold of 5")
	}
	cb.RecordFailure("slow:prov:model")
	if cb.Allow("slow:prov:model") {
		t.Fatal("slow did not trip at the default failure threshold")
	}

	// HalfOpen recovery honors the per-key success threshold: fast needs
	// 2 consecutive successes (the probe request trips after the first).
	cb.mu.Lock()
	fast := cb.circuits["fast:prov:model"]
	fast.expiry = time.Now().Add(-time.Second)
	cb.mu.Unlock()
	if !cb.Allow("fast:prov:model") {
		t.Fatal("fast did not probe after its cooldown expired")
	}
	cb.RecordSuccess("fast:prov:model")
	cb.mu.Lock()
	state := cb.circuits["fast:prov:model"].state
	cb.mu.Unlock()
	if state != StateHalfOpen {
		t.Fatalf("fast recovered after 1 success, want still HalfOpen (per-key success threshold 2): %v", state)
	}
	if !cb.Allow("fast:prov:model") {
		t.Fatal("fast denied the second HalfOpen probe")
	}
	cb.RecordSuccess("fast:prov:model")
	cb.mu.Lock()
	state = cb.circuits["fast:prov:model"].state
	cb.mu.Unlock()
	if state != StateClosed {
		t.Fatalf("fast did not recover after 2 successes (per-key success threshold): %v", state)
	}

	// slow (default success threshold 1) recovers on the first success.
	cb.mu.Lock()
	slow := cb.circuits["slow:prov:model"]
	slow.expiry = time.Now().Add(-time.Second)
	cb.mu.Unlock()
	if !cb.Allow("slow:prov:model") {
		t.Fatal("slow did not probe after its cooldown expired")
	}
	cb.RecordSuccess("slow:prov:model")
	cb.mu.Lock()
	state = cb.circuits["slow:prov:model"].state
	cb.mu.Unlock()
	if state != StateClosed {
		t.Fatalf("slow did not recover after 1 success (default threshold): %v", state)
	}
}

// TestCircuitBreakerThresholdLookupZeroInherits pins the inherit
// semantics: a lookup returning 0 for a field falls back to the global
// value, so agents omitting a threshold are unchanged.
func TestCircuitBreakerThresholdLookupZeroInherits(t *testing.T) {
	cb := NewCircuitBreaker(3, 1, 1, time.Second, nil)
	cb.SetThresholdLookup(func(key string) (failure, success int) {
		return 0, 0
	})
	for i := 0; i < 2; i++ {
		cb.RecordFailure("agent:prov:model")
	}
	if !cb.Allow("agent:prov:model") {
		t.Fatal("tripped before the inherited default of 3")
	}
	cb.RecordFailure("agent:prov:model")
	if cb.Allow("agent:prov:model") {
		t.Fatal("did not trip at the inherited default of 3")
	}
}

// TestCircuitBreakerPerKeyThresholdsOtherSites covers the two remaining
// threshold sites: RecordFailureWithStatus's StateClosed trip (the hot
// retry-loop path) and RecordSuccessWithModel's HalfOpen recovery.
func TestCircuitBreakerPerKeyThresholdsOtherSites(t *testing.T) {
	cb := NewCircuitBreaker(5, 1, 1, time.Second, nil)
	cb.SetThresholdLookup(func(key string) (failure, success int) {
		if key == "fast:prov:model" {
			return 2, 2
		}
		return 0, 0
	})

	cb.RecordFailureWithStatus("fast:prov:model", 503, "")
	if !cb.Allow("fast:prov:model") {
		t.Fatal("fast tripped before its threshold")
	}
	cb.RecordFailureWithStatus("fast:prov:model", 503, "")
	if cb.Allow("fast:prov:model") {
		t.Fatal("fast did not trip at its per-key threshold via RecordFailureWithStatus")
	}

	// Recovery through RecordSuccessWithModel at the per-key threshold.
	cb.mu.Lock()
	fast := cb.circuits["fast:prov:model"]
	fast.expiry = time.Now().Add(-time.Second)
	cb.mu.Unlock()
	if !cb.Allow("fast:prov:model") {
		t.Fatal("fast did not probe after its cooldown expired")
	}
	cb.RecordSuccessWithModel("fast:prov:model", "fast:prov:model")
	cb.mu.Lock()
	state := cb.circuits["fast:prov:model"].state
	cb.mu.Unlock()
	if state != StateHalfOpen {
		t.Fatalf("fast recovered after 1 success, want still HalfOpen: %v", state)
	}
	if !cb.Allow("fast:prov:model") {
		t.Fatal("fast denied the second HalfOpen probe")
	}
	cb.RecordSuccessWithModel("fast:prov:model", "fast:prov:model")
	cb.mu.Lock()
	state = cb.circuits["fast:prov:model"].state
	cb.mu.Unlock()
	if state != StateClosed {
		t.Fatalf("fast did not recover after 2 successes: %v", state)
	}
}
