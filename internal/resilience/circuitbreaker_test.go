package resilience

import (
	"sync"
	"testing"
	"time"
)

func TestNewCircuitBreaker(t *testing.T) {
	cb := NewCircuitBreaker(5, 1, 3, 60*time.Second, nil)
	if cb == nil {
		t.Fatal("NewCircuitBreaker returned nil")
	}
	if cb.failureThreshold != 5 {
		t.Fatalf("expected failureThreshold 5, got %d", cb.failureThreshold)
	}
	if cb.successThreshold != 1 {
		t.Fatalf("expected successThreshold 1, got %d", cb.successThreshold)
	}
	if cb.halfOpenMaxRequests != 3 {
		t.Fatalf("expected halfOpenMaxRequests 3, got %d", cb.halfOpenMaxRequests)
	}
	if cb.cooldown != 60*time.Second {
		t.Fatalf("expected cooldown 60s, got %v", cb.cooldown)
	}
}

func TestNewCircuitBreaker_Defaults(t *testing.T) {
	cb := NewCircuitBreaker(0, 0, 0, 0, nil)
	if cb.failureThreshold != 5 {
		t.Fatalf("expected default failureThreshold 5, got %d", cb.failureThreshold)
	}
	if cb.successThreshold != 1 {
		t.Fatalf("expected default successThreshold 1, got %d", cb.successThreshold)
	}
	if cb.halfOpenMaxRequests != 1 {
		t.Fatalf("expected default halfOpenMaxRequests 1, got %d", cb.halfOpenMaxRequests)
	}
	if cb.cooldown != 60*time.Second {
		t.Fatalf("expected default cooldown 60s, got %v", cb.cooldown)
	}
}

func TestAllow_Closed(t *testing.T) {
	cb := NewCircuitBreaker(5, 1, 3, 60*time.Second, nil)
	if !cb.Allow("test-key") {
		t.Fatal("Allow should return true in Closed state")
	}
	if cb.State("test-key") != StateClosed {
		t.Fatal("state should still be Closed after Allow")
	}
}

func TestRecordFailure_Closed_UnderThreshold(t *testing.T) {
	cb := NewCircuitBreaker(5, 1, 3, 60*time.Second, nil)
	for i := 0; i < 4; i++ {
		cb.RecordFailure("test-key")
	}
	if cb.State("test-key") != StateClosed {
		t.Fatal("state should still be Closed after 4 failures (threshold 5)")
	}
}

func TestRecordFailure_Closed_HitsThreshold(t *testing.T) {
	var lastKey string
	var lastFrom, lastTo State

	onChange := func(key string, from, to State) {
		lastKey = key
		lastFrom = from
		lastTo = to
	}

	cb := NewCircuitBreaker(5, 1, 3, 60*time.Second, onChange)
	for i := 0; i < 5; i++ {
		cb.RecordFailure("test-key")
	}

	if cb.State("test-key") != StateOpen {
		t.Fatal("state should be Open after 5 failures")
	}
	if lastKey != "test-key" {
		t.Fatalf("onChange not called for key %s", lastKey)
	}
	if lastFrom != StateClosed {
		t.Fatalf("expected from StateClosed, got %v", lastFrom)
	}
	if lastTo != StateOpen {
		t.Fatalf("expected to StateOpen, got %v", lastTo)
	}
}

func TestRecordSuccess_Closed_ResetsConsecutiveFailures(t *testing.T) {
	cb := NewCircuitBreaker(5, 1, 3, 60*time.Second, nil)
	cb.RecordFailure("test-key")
	cb.RecordFailure("test-key")
	cb.RecordSuccess("test-key")

	counts := cb.circuits["test-key"].counts
	if counts.ConsecutiveFailures != 0 {
		t.Fatalf("expected ConsecutiveFailures 0 after success, got %d", counts.ConsecutiveFailures)
	}
	if counts.ConsecutiveSuccesses != 1 {
		t.Fatalf("expected ConsecutiveSuccesses 1, got %d", counts.ConsecutiveSuccesses)
	}
}

func TestAllow_Open_BeforeExpiry(t *testing.T) {
	cb := NewCircuitBreaker(5, 1, 3, 1*time.Second, nil)
	for i := 0; i < 5; i++ {
		cb.RecordFailure("test-key")
	}
	if cb.Allow("test-key") {
		t.Fatal("Allow should return false in Open state before expiry")
	}
}

func TestAllow_Open_AfterExpiry_TransitionsToHalfOpen(t *testing.T) {
	var lastKey string
	var lastFrom, lastTo State

	onChange := func(key string, from, to State) {
		lastKey = key
		lastFrom = from
		lastTo = to
	}

	cb := NewCircuitBreaker(5, 1, 3, 10*time.Millisecond, onChange)
	for i := 0; i < 5; i++ {
		cb.RecordFailure("test-key")
	}
	time.Sleep(20 * time.Millisecond)

	if !cb.Allow("test-key") {
		t.Fatal("Allow should return true after cooldown expires")
	}
	if cb.State("test-key") != StateHalfOpen {
		t.Fatalf("expected StateHalfOpen, got %v", cb.State("test-key"))
	}
	if lastKey != "test-key" {
		t.Fatal("onChange not called")
	}
	if lastFrom != StateOpen {
		t.Fatalf("expected from StateOpen, got %v", lastFrom)
	}
	if lastTo != StateHalfOpen {
		t.Fatalf("expected to StateHalfOpen, got %v", lastTo)
	}
}

func TestAllow_HalfOpen_Cap(t *testing.T) {
	cb := NewCircuitBreaker(5, 2, 3, 10*time.Millisecond, nil)
	for i := 0; i < 5; i++ {
		cb.RecordFailure("test-key")
	}
	time.Sleep(20 * time.Millisecond)

	for i := 0; i < 3; i++ {
		if !cb.Allow("test-key") {
			t.Fatalf("Allow %d should return true under cap", i+1)
		}
	}

	if cb.Allow("test-key") {
		t.Fatal("4th Allow should return false (cap 3)")
	}
}

func TestRecordFailure_HalfOpen(t *testing.T) {
	var lastTo State

	onChange := func(key string, from, to State) {
		lastTo = to
	}

	cb := NewCircuitBreaker(5, 2, 3, 10*time.Millisecond, onChange)
	for i := 0; i < 5; i++ {
		cb.RecordFailure("test-key")
	}
	time.Sleep(20 * time.Millisecond)

	cb.Allow("test-key")
	cb.RecordFailure("test-key")

	if cb.State("test-key") != StateOpen {
		t.Fatalf("expected StateOpen after failure in HalfOpen, got %v", cb.State("test-key"))
	}
	if lastTo != StateOpen {
		t.Fatalf("expected transition to StateOpen, got %v", lastTo)
	}
}

func TestRecordSuccess_HalfOpen_UnderThreshold(t *testing.T) {
	cb := NewCircuitBreaker(5, 3, 3, 10*time.Millisecond, nil)
	for i := 0; i < 5; i++ {
		cb.RecordFailure("test-key")
	}
	time.Sleep(20 * time.Millisecond)

	cb.Allow("test-key")
	cb.RecordSuccess("test-key")
	cb.Allow("test-key")
	cb.RecordSuccess("test-key")

	if cb.State("test-key") != StateHalfOpen {
		t.Fatalf("expected StateHalfOpen (threshold 3, only 2 successes), got %v", cb.State("test-key"))
	}
}

func TestRecordSuccess_HalfOpen_HitsThreshold(t *testing.T) {
	var lastTo State

	onChange := func(key string, from, to State) {
		lastTo = to
	}

	cb := NewCircuitBreaker(5, 3, 3, 10*time.Millisecond, onChange)
	for i := 0; i < 5; i++ {
		cb.RecordFailure("test-key")
	}
	time.Sleep(20 * time.Millisecond)

	cb.Allow("test-key")
	cb.RecordSuccess("test-key")
	cb.Allow("test-key")
	cb.RecordSuccess("test-key")
	cb.Allow("test-key")
	cb.RecordSuccess("test-key")

	if cb.State("test-key") != StateClosed {
		t.Fatalf("expected StateClosed after 3 successes, got %v", cb.State("test-key"))
	}
	if lastTo != StateClosed {
		t.Fatalf("expected transition to StateClosed, got %v", lastTo)
	}
}

func TestForceOpen(t *testing.T) {
	var lastTo State

	onChange := func(key string, from, to State) {
		lastTo = to
	}

	cb := NewCircuitBreaker(5, 1, 3, 60*time.Second, onChange)
	cb.ForceOpen("test-key", 30*time.Second)

	if cb.State("test-key") != StateOpen {
		t.Fatalf("expected StateOpen after ForceOpen, got %v", cb.State("test-key"))
	}
	if lastTo != StateOpen {
		t.Fatalf("expected onChange to fire, got %v", lastTo)
	}
	if cb.Allow("test-key") {
		t.Fatal("Allow should return false after ForceOpen")
	}
}

func TestForceOpen_EmptyKey(t *testing.T) {
	cb := NewCircuitBreaker(5, 1, 3, 60*time.Second, nil)
	cb.ForceOpen("", 30*time.Second)
	if len(cb.circuits) != 0 {
		t.Fatal("should not create circuit for empty key")
	}
}

func TestForceOpen_ZeroDuration(t *testing.T) {
	cb := NewCircuitBreaker(5, 1, 3, 60*time.Second, nil)
	cb.RecordFailure("test-key")
	initialState := cb.State("test-key")

	cb.ForceOpen("test-key", 0)
	if cb.State("test-key") != initialState {
		t.Fatal("state should not change with zero duration")
	}
}

func TestActiveCount(t *testing.T) {
	cb := NewCircuitBreaker(5, 1, 3, 1*time.Minute, nil)
	cb.RecordFailure("test-key-1")
	cb.RecordFailure("test-key-2")
	for i := 0; i < 5; i++ {
		cb.RecordFailure("test-key-1")
		cb.RecordFailure("test-key-2")
	}

	if cb.ActiveCount() != 2 {
		t.Fatalf("expected 2 active circuits, got %d", cb.ActiveCount())
	}

	cb.ForceOpen("test-key-3", 1*time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	if cb.ActiveCount() != 2 {
		t.Fatalf("expected 2 active circuits (one expired), got %d", cb.ActiveCount())
	}
}

func TestSnapshot(t *testing.T) {
	cb := NewCircuitBreaker(5, 1, 3, 1*time.Minute, nil)
	for i := 0; i < 5; i++ {
		cb.RecordFailure("test-key")
	}

	snap := cb.Snapshot()
	if snap["test-key"] != "open" {
		t.Fatalf("expected 'open' in snapshot, got %s", snap["test-key"])
	}
}

func TestSnapshot_WithExpiredCircuit(t *testing.T) {
	cb := NewCircuitBreaker(5, 1, 3, 10*time.Millisecond, nil)
	for i := 0; i < 5; i++ {
		cb.RecordFailure("test-key")
	}
	time.Sleep(20 * time.Millisecond)

	snap := cb.Snapshot()
	if snap["test-key"] != "half_open" {
		t.Fatalf("expected 'half_open' for expired circuit, got %s", snap["test-key"])
	}
}

func TestConcurrentAccess(t *testing.T) {
	cb := NewCircuitBreaker(100, 1, 10, 10*time.Millisecond, nil)
	var wg sync.WaitGroup

	workers := 100
	failuresPerWorker := 5
	successesPerWorker := 3

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()
			key := "test-key"
			for j := 0; j < failuresPerWorker; j++ {
				cb.Allow(key)
				cb.RecordFailure(key)
			}
			for j := 0; j < successesPerWorker; j++ {
				time.Sleep(20 * time.Millisecond)
				cb.Allow(key)
				cb.RecordSuccess(key)
			}
		}(i)
	}

	wg.Wait()
	snap := cb.Snapshot()
	if state, ok := snap["test-key"]; !ok {
		t.Fatal("test-key not found in snapshot")
	} else if state != "closed" && state != "open" && state != "half_open" {
		t.Fatalf("unexpected state after concurrent access: %s", state)
	}
}

func TestRecordFailure_CooldownOverride(t *testing.T) {
	cb := NewCircuitBreaker(5, 1, 3, 1*time.Minute, nil)
	for i := 0; i < 5; i++ {
		cb.RecordFailure("test-key", 5*time.Second)
	}

	c := cb.circuits["test-key"]
	expectedExpiry := time.Now().Add(5 * time.Second)
	if c.expiry.Before(expectedExpiry.Add(-time.Second)) || c.expiry.After(expectedExpiry.Add(time.Second)) {
		t.Fatalf("cooldown override not respected: expected ~5s, got %v", c.expiry.Sub(time.Now()))
	}
}

func TestRecordFailure_AlreadyOpen_ExtendsCooldown(t *testing.T) {
	cb := NewCircuitBreaker(5, 1, 3, 1*time.Minute, nil)
	for i := 0; i < 5; i++ {
		cb.RecordFailure("test-key")
	}
	c := cb.circuits["test-key"]
	initialExpiry := c.expiry

	time.Sleep(100 * time.Millisecond)
	cb.RecordFailure("test-key", 2*time.Minute)

	if c.expiry.Before(initialExpiry) {
		t.Fatal("cooldown should not be reduced")
	}
	expectedExtension := time.Minute - 100*time.Millisecond
	if c.expiry.Sub(initialExpiry) < expectedExtension-5*time.Second {
		t.Fatalf("cooldown should be extended by ~60s, got %v", c.expiry.Sub(initialExpiry))
	}
}
