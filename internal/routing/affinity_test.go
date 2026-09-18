package routing

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func affinityPayload(turnContents ...string) map[string]any {
	msgs := make([]any, len(turnContents))
	roles := []string{"user", "assistant"}
	for i, c := range turnContents {
		msgs[i] = map[string]any{"role": roles[i%2], "content": c}
	}
	return map[string]any{"messages": msgs}
}

// TestBuildAffinityChain_PrefixExtension pins the incremental property: a
// longer conversation's chain extends the shorter one's chain unchanged, so
// LCP comparisons behave like turn-prefix comparisons.
func TestBuildAffinityChain_PrefixExtension(t *testing.T) {
	short := BuildAffinityChain(affinityPayload("hello"))
	long := BuildAffinityChain(affinityPayload("hello", "hi there", "continue"))

	if len(short) != 1 || len(long) != 3 {
		t.Fatalf("expected chain lengths 1 and 3, got %d and %d", len(short), len(long))
	}
	if short[0] != long[0] {
		t.Error("prefix chains must share leading fingerprints")
	}
}

// TestBuildAffinityChain_DeterministicAndDistinct pins determinism for
// identical payloads and divergence for different continuations.
func TestBuildAffinityChain_DeterministicAndDistinct(t *testing.T) {
	a1 := BuildAffinityChain(affinityPayload("hi", "reply A"))
	a2 := BuildAffinityChain(affinityPayload("hi", "reply A"))
	b := BuildAffinityChain(affinityPayload("hi", "reply B"))

	if a1[1] != a2[1] {
		t.Error("identical payloads must produce identical chains")
	}
	if a1[1] == b[1] {
		t.Error("different continuations must diverge at turn 2")
	}
}

// TestBuildAffinityChain_SamplingBounded pins the CPU/memory bound: hashing
// a large history costs O(turns × sample), not O(history) — 2000 turns of
// 8KB content must build well under a second (naive full hashing would be
// ~16MB; the point is that growth is linear and capped at 64 turns).
func TestBuildAffinityChain_SamplingBounded(t *testing.T) {
	big := make([]string, 2000)
	for i := range big {
		big[i] = fmt.Sprintf("turn %d %s", i, string(make([]byte, 8<<10)))
	}
	start := time.Now()
	chain := BuildAffinityChain(affinityPayload(big...))
	elapsed := time.Since(start)

	if len(chain) != affinityChainDepth {
		t.Fatalf("chain must cap at %d turns, got %d", affinityChainDepth, len(chain))
	}
	if elapsed > 2*time.Second {
		t.Fatalf("chain build too slow: %v", elapsed)
	}
}

// TestResolveIdentity_ContinuationAndCollision pins the NENYA-39 matcher:
// continuations keep the root identity, conversations sharing only the
// opener fork deterministically, and forks stay stable across repeats.
func TestResolveIdentity_ContinuationAndCollision(t *testing.T) {
	s := NewAffinityStore(0, 0)

	// Session A starts, then continues: identity stable and extended.
	rootA := "root-a"
	if got := s.ResolveIdentity(rootA, BuildAffinityChain(affinityPayload("hi"))); got != rootA {
		t.Fatalf("fresh key must register unchanged, got %s", got)
	}
	cont := BuildAffinityChain(affinityPayload("hi", "answer"))
	if got := s.ResolveIdentity(rootA, cont); got != rootA {
		t.Fatalf("1-turn remembered depth must match on the opener, got %s", got)
	}

	// Conversation B shares ONLY the opener with A's deeper history:
	// shared turns = 1 < 2 → fork with a deterministic identity.
	bChain := BuildAffinityChain(affinityPayload("hi", "totally different"))
	forkB := s.ResolveIdentity(rootA, bChain)
	if forkB == rootA {
		t.Fatal("collision must fork, not inherit")
	}
	if again := s.ResolveIdentity(rootA, bChain); again != forkB {
		t.Fatalf("fork identity must be stable across repeats: %s vs %s", again, forkB)
	}

	// A continues: untouched by the fork.
	if got := s.ResolveIdentity(rootA, cont); got != rootA {
		t.Fatalf("original session must keep its identity, got %s", got)
	}

	// A fork of A's full history (same turns) is the same conversation:
	// exact identity, no fork.
	if got := s.ResolveIdentity(rootA, cont); got != rootA {
		t.Fatalf("identical history must not fork, got %s", got)
	}
}

// TestResolveIdentity_FreshKeyUnaffected pins that unrelated conversations
// with distinct openers never interact.
func TestResolveIdentity_FreshKeyUnaffected(t *testing.T) {
	s := NewAffinityStore(0, 0)
	k1 := s.ResolveIdentity("root-1", BuildAffinityChain(affinityPayload("weather in SF")))
	k2 := s.ResolveIdentity("root-2", BuildAffinityChain(affinityPayload("write a poem")))
	if k1 != "root-1" || k2 != "root-2" {
		t.Fatalf("distinct openers must keep distinct identities: %s, %s", k1, k2)
	}
	if s.Len() != 2 {
		t.Fatalf("expected 2 remembered sessions, got %d", s.Len())
	}
}

// TestAffinityStore_LRUBound pins the bounded flat KV: inserting past cap
// evicts least-recently-used entries.
func TestAffinityStore_LRUBound(t *testing.T) {
	s := NewAffinityStore(2, 0)
	s.ResolveIdentity("k1", BuildAffinityChain(affinityPayload("a")))
	s.ResolveIdentity("k2", BuildAffinityChain(affinityPayload("b")))
	// Touch k1 so k2 becomes the LRU entry.
	s.ResolveIdentity("k1", BuildAffinityChain(affinityPayload("a")))
	s.ResolveIdentity("k3", BuildAffinityChain(affinityPayload("c")))

	if s.Len() != 2 {
		t.Fatalf("cap must bound entries, got %d", s.Len())
	}
	// k2 was evicted: re-resolving registers fresh.
	if got := s.ResolveIdentity("k2", BuildAffinityChain(affinityPayload("b"))); got != "k2" {
		t.Fatalf("re-register after eviction must return root, got %s", got)
	}
}

// TestAffinityStore_Concurrent pins race-safety of resolution.
func TestAffinityStore_Concurrent(t *testing.T) {
	s := NewAffinityStore(0, 0)
	chain := BuildAffinityChain(affinityPayload("shared opener", "continuation"))
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.ResolveIdentity(fmt.Sprintf("root-%d", i%4), chain)
		}(i)
	}
	wg.Wait()
}
