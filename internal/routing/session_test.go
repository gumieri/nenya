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
