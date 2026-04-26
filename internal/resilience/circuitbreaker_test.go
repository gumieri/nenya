package resilience

import (
	"testing"
	"time"
)

func TestCircuitBreaker_AllowClosed(t *testing.T) {
	cb := NewCircuitBreaker(2, 1, 1, time.Second, nil)

	// First request allowed
	if !cb.Allow("test") {
		t.Error("expected first request to be allowed")
	}

	// Second request allowed
	if !cb.Allow("test") {
		t.Error("expected second request to be allowed")
	}
}

func TestCircuitBreaker_TripOnFailures(t *testing.T) {
	cb := NewCircuitBreaker(2, 1, 1, time.Second, nil)
	key := "test"

	// Record 2 failures to trip the breaker
	cb.RecordFailure(key)
	cb.RecordFailure(key)

	// Next request should be blocked
	if cb.Allow(key) {
		t.Error("expected request to be blocked after tripping")
	}
}

func TestCircuitBreaker_HalfOpenRecovery(t *testing.T) {
	cb := NewCircuitBreaker(2, 1, 1, 10*time.Millisecond, nil)
	key := "test"

	// Trip the breaker
	cb.RecordFailure(key)
	cb.RecordFailure(key)

	// Wait for cooldown
	time.Sleep(15 * time.Millisecond)

	// First request in half-open should be allowed
	if !cb.Allow(key) {
		t.Error("expected half-open request to be allowed")
	}

	// Second request should be blocked
	if cb.Allow(key) {
		t.Error("expected second half-open request to be blocked")
	}

	// Record success to close the circuit
	cb.RecordSuccess(key)

	// Circuit should now be closed
	if !cb.Peek(key) {
		t.Error("expected circuit to be closed after success")
	}
}

func TestCircuitBreaker_HalfOpenFailure(t *testing.T) {
	cb := NewCircuitBreaker(2, 1, 1, 10*time.Millisecond, nil)
	key := "test"

	// Trip the breaker
	cb.RecordFailure(key)
	cb.RecordFailure(key)

	// Wait for cooldown
	time.Sleep(15 * time.Millisecond)

	// Allow the half-open request
	cb.Allow(key)

	// Record failure during half-open
	cb.RecordFailure(key)

	// Circuit should be open again
	if cb.Peek(key) {
		t.Error("expected circuit to remain open after half-open failure")
	}
}

func TestCircuitBreaker_ForceOpen(t *testing.T) {
	cb := NewCircuitBreaker(1, 1, 1, time.Hour, nil)
	key := "test"

	// Force open the circuit
	cb.ForceOpen(key, time.Hour)

	// Circuit should be open
	if cb.Peek(key) {
		t.Error("expected circuit to be open")
	}
}

func TestCircuitBreaker_StateTransitions(t *testing.T) {
	cb := NewCircuitBreaker(2, 1, 1, time.Second, nil)
	key := "test"

	// Initially closed
	if cb.State(key) != StateClosed {
		t.Errorf("expected StateClosed, got %v", cb.State(key))
	}

	// Trip the breaker
	cb.RecordFailure(key)
	cb.RecordFailure(key)

	// Should be open
	if cb.State(key) != StateOpen {
		t.Errorf("expected StateOpen, got %v", cb.State(key))
	}

	// Wait for cooldown and allow half-open
	time.Sleep(time.Second + 10*time.Millisecond)
	cb.Allow(key)

	if cb.State(key) != StateHalfOpen {
		t.Errorf("expected StateHalfOpen, got %v", cb.State(key))
	}

	// Record success to close
	cb.RecordSuccess(key)

	// Should be closed again
	if cb.State(key) != StateClosed {
		t.Errorf("expected StateClosed, got %v", cb.State(key))
	}
}
