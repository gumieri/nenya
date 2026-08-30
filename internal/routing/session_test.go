package routing

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSessionKeyDeterministic(t *testing.T) {
	k1 := SessionKey("agent", "sys", "hello")
	k2 := SessionKey("agent", "sys", "hello")
	if k1 != k2 {
		t.Fatalf("session key not deterministic")
	}
	k3 := SessionKey("agent", "sys", "goodbye")
	if k1 == k3 {
		t.Fatalf("different inputs produced same key")
	}
}

func TestSessionRouterPinLookup(t *testing.T) {
	sr := NewSessionRouter(10, 1*time.Hour, nil, nil)
	sr.Pin("k1", "p1", "m1", "a1", 0)
	state, ok := sr.Lookup("k1")
	if !ok {
		t.Fatalf("pin not found")
	}
	if state.Provider != "p1" || state.Model != "m1" || state.Account != "a1" {
		t.Fatalf("pin data mismatch")
	}
}

func TestSessionRouterTTLExpiry(t *testing.T) {
	sr := NewSessionRouter(10, 50*time.Millisecond, nil, nil)
	sr.Pin("k1", "p1", "m1", "a1", 0)
	if _, ok := sr.Lookup("k1"); !ok {
		t.Fatalf("pin not found immediately")
	}
	time.Sleep(60 * time.Millisecond)
	if _, ok := sr.Lookup("k1"); ok {
		t.Fatalf("pin expired but still present")
	}
}

func TestSessionRouterLRUEviction(t *testing.T) {
	sr := NewSessionRouter(2, 1*time.Hour, nil, nil)
	sr.Pin("k1", "p1", "m1", "a1", 0)
	sr.Pin("k2", "p2", "m2", "a2", 0)
	if sr.Len() != 2 {
		t.Fatalf("len should be 2, got %d", sr.Len())
	}
	sr.Pin("k3", "p3", "m3", "a3", 0)
	if _, ok := sr.Lookup("k1"); ok {
		t.Fatalf("LRU pin not evicted")
	}
	if sr.Len() != 2 {
		t.Fatalf("len should be 2 after eviction, got %d", sr.Len())
	}
}

func TestSessionRouterPromote(t *testing.T) {
	sr := NewSessionRouter(10, 1*time.Hour, nil, nil)
	sr.Pin("k1", "p1", "m1", "a1", 0)
	state, ok := sr.Lookup("k1")
	if !ok {
		t.Fatalf("pin not found")
	}
	sr.Promote("k1", "p2", "m2", "a2", 0)
	state, ok = sr.Lookup("k1")
	if !ok {
		t.Fatalf("pin lost after promote")
	}
	if state.Provider != "p2" || state.Model != "m2" {
		t.Fatalf("promote did not overwrite")
	}
}

func TestSessionRouterPeek(t *testing.T) {
	sr := NewSessionRouter(10, 1*time.Hour, nil, nil)
	if _, ok := sr.Peek("missing"); ok {
		t.Fatal("expected absent pin for unknown key")
	}

	sr.Pin("k1", "p1", "m1", "a1", 0)
	pin, ok := sr.Peek("k1")
	if !ok || pin.Provider != "p1" || pin.Model != "m1" || pin.Account != "a1" {
		t.Fatalf("unexpected peek: %+v, %v", pin, ok)
	}

	// Peek reports expired pins as absent without deleting them or recording
	// metrics; the subsequent Lookup performs the destructive expiry.
	sr.Pin("k2", "p2", "m2", "a2", 50*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	if _, ok := sr.Peek("k2"); ok {
		t.Fatal("expected expired pin to be reported absent by Peek")
	}
	if _, ok := sr.Lookup("k2"); ok {
		t.Fatal("expected expired pin to be reported absent by Lookup")
	}

	// Peek on a live pin leaves LastSeen untouched.
	sr2 := NewSessionRouter(10, 1*time.Hour, nil, nil)
	sr2.Pin("k1", "p1", "m1", "a1", 0)
	before, _ := sr2.Peek("k1")
	sr2.Peek("k1")
	sr2.Peek("k1")
	after, _ := sr2.Peek("k1")
	if !after.LastSeen.Equal(before.LastSeen) {
		t.Fatal("Peek must not refresh LastSeen")
	}
}

func TestSessionRouterPromoteIfChanged(t *testing.T) {
	sr := NewSessionRouter(10, 1*time.Hour, nil, nil)
	sr.Pin("k1", "p1", "m1", "a1", 0)
	before, _ := sr.Lookup("k1")

	// Identical target: no change, no Since reset.
	if sr.PromoteIfChanged("k1", "p1", "m1", "a1", 0) {
		t.Fatal("expected false for unchanged pin")
	}
	after, ok := sr.Lookup("k1")
	if !ok || !after.Since.Equal(before.Since) {
		t.Fatalf("unchanged pin must be untouched, got %+v (before Since %v)", after, before.Since)
	}

	// Account change only.
	if !sr.PromoteIfChanged("k1", "p1", "m1", "a2", 0) {
		t.Fatal("expected true for account change")
	}
	state, _ := sr.Lookup("k1")
	if state.Account != "a2" || state.Provider != "p1" || state.Model != "m1" {
		t.Fatalf("unexpected pin after account change: %+v", state)
	}

	// Provider/model change.
	if !sr.PromoteIfChanged("k1", "p2", "m2", "a2", 0) {
		t.Fatal("expected true for provider/model change")
	}
	state, _ = sr.Lookup("k1")
	if state.Provider != "p2" || state.Model != "m2" {
		t.Fatalf("unexpected pin after provider/model change: %+v", state)
	}

	// Nil router is a safe no-op.
	var nilRouter *SessionRouter
	if nilRouter.PromoteIfChanged("k", "p", "m", "a", 0) {
		t.Fatal("nil router must report no change")
	}
}

func TestSessionRouterEvict(t *testing.T) {
	sr := NewSessionRouter(10, 1*time.Hour, nil, nil)
	sr.Pin("k1", "p1", "m1", "a1", 0)
	sr.Evict("k1")
	if _, ok := sr.Lookup("k1"); ok {
		t.Fatalf("pin not removed")
	}
}

func TestSessionRouterActive(t *testing.T) {
	sr := NewSessionRouter(10, 50*time.Millisecond, nil, nil)
	sr.Pin("k1", "p1", "m1", "a1", 0)
	sr.Pin("k2", "p2", "m2", "a2", 0)
	if active := sr.Active(); active != 2 {
		t.Fatalf("active count should be 2, got %d", active)
	}
	time.Sleep(60 * time.Millisecond)
	if active := sr.Active(); active != 0 {
		t.Fatalf("active count should be 0 after expiry, got %d", active)
	}
}

func TestSessionRouterConcurrentAccess(t *testing.T) {
	sr := NewSessionRouter(1024, 1*time.Hour, nil, nil)
	const goroutines = 16
	const keysPerGoroutine = 64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < keysPerGoroutine; i++ {
				key := fmt.Sprintf("g%d-k%d", g, i)
				sr.Pin(key, "p1", "m1", "a1", 0)
				state, ok := sr.Lookup(key)
				if !ok || state.Provider != "p1" {
					t.Errorf("lookup failed for %s", key)
					return
				}
				if (g+i)%4 == 0 {
					sr.Evict(key)
				}
			}
			sr.Active()
			sr.Len()
		}(g)
	}
	wg.Wait()
}

func TestSessionRouterConcurrentEviction(t *testing.T) {
	sr := NewSessionRouter(128, 1*time.Hour, nil, nil)
	const goroutines = 8
	const keysPerGoroutine = 64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < keysPerGoroutine; i++ {
				key := fmt.Sprintf("g%d-k%d", g, i)
				sr.Pin(key, "p1", "m1", "a1", 0)
			}
		}(g)
	}
	wg.Wait()
	if active := sr.Active(); active > sr.cap {
		t.Fatalf("active count %d exceeds cap %d", active, sr.cap)
	}
}
