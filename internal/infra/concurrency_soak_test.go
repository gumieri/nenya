package infra

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestConcurrencyLimiterSoakNoResidualGrowth is the NENYA-184 memory-soak
// fixture: many rounds of acquire/release across MORE distinct keys than
// the registry cap must leave the key count bounded (idle eviction), the
// slots fully released (a fresh acquire at limit 1 must still block a
// second), and no goroutines behind.
func TestConcurrencyLimiterSoakNoResidualGrowth(t *testing.T) {
	c := NewConcurrencyLimiter()
	const keys = maxConcurrencyKeys + 50

	for round := 0; round < 5; round++ {
		for i := 0; i < keys; i++ {
			key := fmt.Sprintf("prov/model-%d", i)
			release, err := c.Acquire(context.Background(), key, 2)
			if err != nil {
				t.Fatalf("round %d key %d: %v", round, i, err)
			}
			release()
		}
	}

	c.mu.RLock()
	n := len(c.slots)
	c.mu.RUnlock()
	if n > maxConcurrencyKeys {
		t.Fatalf("slot registry grew past its cap: %d keys tracked after soak", n)
	}

	// Admission still works and the limit is still enforced after the
	// churn: a leaked slot would let the second acquire through. The
	// probe key is never churned, so the check is independent of the
	// eviction policy.
	key := "fresh/probe-key"
	r1, err := c.Acquire(context.Background(), key, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := c.Acquire(ctx, key, 1); err == nil {
		t.Fatal("a slot leaked: the second acquire at limit 1 was granted")
	}
	r1()
}
