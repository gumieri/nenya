package infra

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrencyLimiter_UnlimitedIsNoOp(t *testing.T) {
	c := NewConcurrencyLimiter()
	release, err := c.Acquire(context.Background(), "p/m", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	release()
	release() // idempotent
	if got := c.Inflight("p/m"); got != 0 {
		t.Fatalf("inflight = %d, want 0 (no slot should exist)", got)
	}
}

func TestConcurrencyLimiter_AcquireRelease(t *testing.T) {
	c := NewConcurrencyLimiter()
	release, err := c.Acquire(context.Background(), "zai/glm-5.3", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := c.Inflight("zai/glm-5.3"); got != 1 {
		t.Fatalf("inflight = %d, want 1", got)
	}
	release()
	if got := c.Inflight("zai/glm-5.3"); got != 0 {
		t.Fatalf("inflight after release = %d, want 0", got)
	}
}

func TestConcurrencyLimiter_BlocksAtLimitThenReleases(t *testing.T) {
	c := NewConcurrencyLimiter()
	ctx := context.Background()

	r1, err := c.Acquire(ctx, "k", 1)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	acquired := make(chan func(), 1)
	go func() {
		r2, err := c.Acquire(ctx, "k", 1)
		if err != nil {
			acquired <- nil
			return
		}
		acquired <- r2
	}()

	select {
	case r2 := <-acquired:
		if r2 != nil {
			t.Fatal("second acquire should block at limit=1")
		}
	case <-time.After(50 * time.Millisecond):
	}

	r1() // free the slot
	var r2 func()
	select {
	case r2 = <-acquired:
	case <-time.After(time.Second):
		t.Fatal("second acquire did not unblock after release")
	}
	if r2 == nil {
		t.Fatal("second acquire returned nil release")
	}
	if got := c.Inflight("k"); got != 1 {
		t.Fatalf("inflight after re-acquire = %d, want 1", got)
	}
	r2()
	if got := c.Inflight("k"); got != 0 {
		t.Fatalf("inflight = %d, want 0", got)
	}
}

func TestConcurrencyLimiter_CtxCancelWhileWaiting(t *testing.T) {
	c := NewConcurrencyLimiter()
	r1, err := c.Acquire(context.Background(), "k", 1)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer r1()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := c.Acquire(ctx, "k", 1); err == nil {
		t.Fatal("expected context error while waiting")
	}
	// The aborted waiter must not hold or leak a slot.
	if got := c.Inflight("k"); got != 1 {
		t.Fatalf("inflight = %d, want 1", got)
	}
}

func TestConcurrencyLimiter_IndependentKeys(t *testing.T) {
	c := NewConcurrencyLimiter()
	r1, err := c.Acquire(context.Background(), "a", 1)
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	defer r1()
	r2, err := c.Acquire(context.Background(), "b", 1)
	if err != nil {
		t.Fatalf("acquire b should not block on key a: %v", err)
	}
	defer r2()
	snap := c.Snapshot()
	if snap["a"] != 1 || snap["b"] != 1 {
		t.Fatalf("snapshot = %v, want a=1 b=1", snap)
	}
}

func TestConcurrencyLimiter_ConcurrentAcquireRelease(t *testing.T) {
	c := NewConcurrencyLimiter()
	const (
		workers = 20
		limit   = 4
		loops   = 50
	)
	var (
		wg      sync.WaitGroup
		maxSeen int32
	)
	acquireSample := func() {
		cur := int32(c.Inflight("k"))
		for {
			prev := atomic.LoadInt32(&maxSeen)
			if cur <= prev || atomic.CompareAndSwapInt32(&maxSeen, prev, cur) {
				return
			}
		}
	}

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range loops {
				r, err := c.Acquire(context.Background(), "k", limit)
				if err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				acquireSample()
				r()
			}
		}()
	}
	wg.Wait()

	if max := int(maxSeen); max > limit {
		t.Fatalf("observed inflight %d exceeds limit %d", max, limit)
	}
	if got := c.Inflight("k"); got != 0 {
		t.Fatalf("inflight after all releases = %d, want 0", got)
	}
}

func TestConcurrencyLimiter_NilReceiverIsSafe(t *testing.T) {
	var c *ConcurrencyLimiter
	release, err := c.Acquire(context.Background(), "k", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	release()
	if got := c.Inflight("k"); got != 0 {
		t.Fatalf("inflight = %d, want 0", got)
	}
	if c.Snapshot() != nil {
		t.Fatal("expected nil snapshot from nil receiver")
	}
}
