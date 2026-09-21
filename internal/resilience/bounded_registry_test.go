package resilience

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func newBoundedCB(max int, ttl time.Duration) *CircuitBreaker {
	cb := NewCircuitBreaker(3, 1, 2, time.Minute, nil)
	cb.SetRegistryLimits(max, ttl)
	return cb
}

// TestRegistry_BoundedUnderChurn pins NENYA-31: flooding the registry with
// distinct adversarial keys cannot grow it past the cap.
func TestRegistry_BoundedUnderChurn(t *testing.T) {
	cb := newBoundedCB(64, time.Minute)
	for i := 0; i < 10000; i++ {
		cb.Allow(fmt.Sprintf("agent/p/m%d", i))
		cb.RecordSuccess(fmt.Sprintf("agent/p/m%d", i))
	}
	size, _ := cb.RegistryStats()
	if size > 64 {
		t.Fatalf("registry exceeded cap: %d circuits", size)
	}
	if _, evictions := cb.RegistryStats(); evictions == 0 {
		t.Error("expected evictions to be recorded")
	}
}

// TestRegistry_OpenCircuitNeverEvicted pins the safety rule: a circuit in
// the middle of its investigation (Open/HalfOpen, or holding failures) is
// never shed to make room — closing it would hide an active failure state.
func TestRegistry_OpenCircuitNeverEvicted(t *testing.T) {
	cb := newBoundedCB(8, 0) // idleTTL 0: everything Closed is immediately idle
	// Trip the protected circuit open.
	for i := 0; i < 3; i++ {
		cb.RecordFailureWithStatus("agent/p/critical", 500, "boom")
	}
	if got := cb.Allow("agent/p/critical"); got {
		t.Fatal("circuit should be open")
	}

	// Churn far past the cap with idle-closed circuits.
	for i := 0; i < 500; i++ {
		key := fmt.Sprintf("agent/p/filler%d", i)
		cb.Allow(key)
		cb.RecordSuccess(key)
	}

	if _, ok := cb.circuits["agent/p/critical"]; !ok {
		t.Fatal("open circuit was evicted — active state lost")
	}
}

// TestRegistry_IdleTTLEviction pins the lazy sweep: a Closed circuit
// untouched past the TTL is evicted once capacity is needed; a recently
// used one survives.
func TestRegistry_IdleTTLEviction(t *testing.T) {
	cb := newBoundedCB(4, 10*time.Millisecond)

	// Fill the registry.
	for i := 0; i < 4; i++ {
		key := fmt.Sprintf("k%d", i)
		cb.Allow(key)
		cb.RecordSuccess(key)
	}
	time.Sleep(25 * time.Millisecond) // everything goes idle

	// Recent touch keeps k3 fresh.
	cb.Allow("k3")

	// One more key forces a sweep: an idle circuit must go, k3 must stay.
	cb.Allow("k-new")
	cb.RecordSuccess("k-new")

	if _, ok := cb.circuits["k3"]; !ok {
		t.Error("recently used circuit was evicted")
	}
	size, _ := cb.RegistryStats()
	if size > 4 {
		t.Fatalf("registry exceeded cap after sweep: %d", size)
	}
}

// TestRegistry_ModelLockPruning checks expired modelLocks entries are
// pruned during eviction sweeps instead of accumulating forever.
func TestRegistry_ModelLockPruning(t *testing.T) {
	cb := newBoundedCB(8, 10*time.Millisecond)
	// Seed model locks directly (no public setter; the map is mutex-guarded).
	cb.mu.Lock()
	cb.modelLocks["m1"] = time.Now().Add(-time.Hour) // already expired
	cb.modelLocks["m2"] = time.Now().Add(time.Hour)  // live
	cb.mu.Unlock()

	for i := 0; i < 8; i++ {
		key := fmt.Sprintf("k%d", i)
		cb.Allow(key)
		cb.RecordSuccess(key)
	}
	cb.Allow("k-new") // triggers a sweep

	cb.mu.Lock()
	_, m1Gone := cb.modelLocks["m1"]
	_, m2Alive := cb.modelLocks["m2"]
	cb.mu.Unlock()
	if m1Gone {
		t.Error("expired lock was not pruned")
	}
	if !m2Alive {
		t.Error("live lock was incorrectly pruned")
	}
}

// TestRegistry_ConcurrentChurn hammers creation/eviction from many
// goroutines under -race.
func TestRegistry_ConcurrentChurn(t *testing.T) {
	cb := newBoundedCB(32, time.Minute)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				key := fmt.Sprintf("g%d/m%d", g, i)
				cb.Allow(key)
				cb.RecordSuccess(key)
				cb.RecordFailureWithStatus(key, 503, "flap")
				_ = cb.Allow(key)
			}
		}(g)
	}
	wg.Wait()

	size, _ := cb.RegistryStats()
	if size > 32 {
		t.Fatalf("registry exceeded cap under concurrency: %d", size)
	}
}
