package infra

import (
	"testing"
	"time"
)

// TestThoughtSignatureCache_ScopedIsolation pins the NENYA-189 IDOR fix:
// upstream-issued tool-call ids cached while serving one API key must not be
// addressable by another key presenting the same id. Scoped views are the
// only form the request paths use (see proxy thoughtSigScope).
func TestThoughtSignatureCache_ScopedIsolation(t *testing.T) {
	cache := NewThoughtSignatureCache(0, 0)
	keyA := cache.Scoped("key-a")
	keyB := cache.Scoped("key-b")

	keyA.Store("call_abc", map[string]string{"thought_signature": "sig-a"})

	// The owning scope reads its own entry back.
	got, ok := keyA.Load("call_abc")
	if !ok {
		t.Fatal("owning scope lost its entry")
	}
	sig, ok := got.(map[string]string)
	if !ok || sig["thought_signature"] != "sig-a" {
		t.Fatalf("owning scope read = %v, want the stored signature", got)
	}

	// A different scope presenting the same upstream-issued id misses.
	if _, ok := keyB.Load("call_abc"); ok {
		t.Fatal("cross-key scope read another key's cached thought signature")
	}

	// The unscoped base cache must not leak the scoped entry either.
	if _, ok := cache.Load("call_abc"); ok {
		t.Fatal("scoped entry visible through the unscoped cache")
	}

	// The same id under a third scope is independent state.
	keyC := cache.Scoped("key-c")
	keyC.Store("call_abc", "sig-c")
	if got, ok := keyA.Load("call_abc"); ok {
		if sig, ok := got.(map[string]string); !ok || sig["thought_signature"] != "sig-a" {
			t.Fatalf("scope C write clobbered scope A: %v", got)
		}
	} else {
		t.Fatal("scope A entry disappeared")
	}
}

// TestThoughtSignatureCache_ScopedEmptyScopeNamespace verifies the empty
// scope is a namespace of its own and never collides with a named scope.
func TestThoughtSignatureCache_ScopedEmptyScopeNamespace(t *testing.T) {
	cache := NewThoughtSignatureCache(0, 0)
	anon := cache.Scoped("")
	named := cache.Scoped("primary")

	anon.Store("call_1", "anon-sig")
	if _, ok := named.Load("call_1"); ok {
		t.Fatal("anonymous-scope entry visible to a named scope")
	}
	if got, ok := anon.Load("call_1"); !ok || got != "anon-sig" {
		t.Fatalf("anonymous scope read = %v, %v; want anon-sig, true", got, ok)
	}
}

// TestThoughtSignatureCache_KVInterface pins that the concrete cache and the
// scoped view both satisfy ThoughtSignatureKV — the type the request paths
// (SanitizeDeps, response transformers, buffered response caching) accept.
func TestThoughtSignatureCache_KVInterface(t *testing.T) {
	cache := NewThoughtSignatureCache(0, 0)
	var _ ThoughtSignatureKV = cache
	var _ ThoughtSignatureKV = cache.Scoped("s")
}

// TestThoughtSignatureCache_TTLAndLRU covers the live behaviors underneath
// the scoped views: expiry, LRU eviction at maxSize, overwrite-refresh, and
// the nil/empty-key contract.
func TestThoughtSignatureCache_TTLAndLRU(t *testing.T) {
	t.Run("nil and empty keys are ignored", func(t *testing.T) {
		cache := NewThoughtSignatureCache(0, 0)
		cache.Store("", "v")
		cache.Store("k", nil)
		if cache.Len() != 0 {
			t.Fatalf("Len = %d, want 0", cache.Len())
		}
		scoped := cache.Scoped("s")
		scoped.Store("", "v")
		scoped.Store("k", nil)
		if _, ok := scoped.Load(""); ok {
			t.Fatal("scoped empty key must be a no-op, not a phantom entry")
		}
		if _, ok := scoped.Load("k"); ok {
			t.Fatal("scoped nil value must be a no-op")
		}
	})

	t.Run("ttl expiry", func(t *testing.T) {
		cache := NewThoughtSignatureCache(0, 1) // 1ns TTL
		cache.Store("k", "v")
		if _, ok := cache.Load("k"); ok {
			t.Fatal("entry must be expired with a 1ns TTL")
		}
		if cache.Len() != 0 {
			t.Fatalf("Len = %d after expiry, want 0", cache.Len())
		}
	})

	t.Run("lru eviction at maxSize", func(t *testing.T) {
		cache := NewThoughtSignatureCache(2, time.Hour)
		cache.Store("a", 1)
		cache.Store("b", 2)
		cache.Load("a") // refresh a: b becomes LRU
		cache.Store("c", 3)
		if _, ok := cache.Load("b"); ok {
			t.Fatal("LRU victim b must have been evicted")
		}
		for _, k := range []string{"a", "c"} {
			if _, ok := cache.Load(k); !ok {
				t.Fatalf("survivor %q evicted", k)
			}
		}
		if cache.Len() != 2 {
			t.Fatalf("Len = %d, want 2", cache.Len())
		}
	})

	t.Run("store overwrite refreshes value and TTL", func(t *testing.T) {
		cache := NewThoughtSignatureCache(0, time.Hour)
		cache.Store("k", "v1")
		cache.Store("k", "v2")
		if cache.Len() != 1 {
			t.Fatalf("Len = %d after overwrite, want 1", cache.Len())
		}
		if got, ok := cache.Load("k"); !ok || got != "v2" {
			t.Fatalf("Load = %v, %v; want v2, true", got, ok)
		}
	})

	t.Run("scoped views share the parent bound", func(t *testing.T) {
		cache := NewThoughtSignatureCache(2, time.Hour)
		cache.Scoped("a").Store("k1", 1)
		cache.Scoped("b").Store("k2", 2)
		cache.Scoped("c").Store("k3", 3)
		if cache.Len() != 2 {
			t.Fatalf("Len = %d, want 2 (scopes share the parent bound)", cache.Len())
		}
	})
}
