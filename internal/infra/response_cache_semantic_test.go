package infra

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// okEmbedder is the minimal non-nil EmbeddingProvider satisfying the
// semantic-mode gate; the tests inject the actual vectors through the Lookup
// embed closures so similarity is exact.
type okEmbedder struct{}

func (okEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{}, nil
}

func testPayload(text string) map[string]interface{} {
	return map[string]interface{}{
		"model":    "m",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": text}},
	}
}

// TestResponseCache_SemanticPartitionedByAuth pins the NENYA-189 tenant
// isolation on the semantic path: the exact-match key already mixes the auth
// hash, and the semantic index must be partitioned by the same identity, so a
// near-identical prompt from API key B can never be served key A's cached
// response.
func TestResponseCache_SemanticPartitionedByAuth(t *testing.T) {
	c := NewResponseCache(16, 0, 0, 0, nil, nil, true, 0.9, okEmbedder{})
	t.Cleanup(c.Stop)

	payload := testPayload("hello world")
	keyA := FingerprintPayloadWithAuth(payload, "token-a")
	keyB := FingerprintPayloadWithAuth(payload, "token-b")
	if keyA == keyB {
		t.Fatal("auth-scoped fingerprints must differ across keys")
	}
	if !strings.HasPrefix(keyA, "auth:") || !strings.HasPrefix(keyB, "auth:") {
		t.Fatalf("authed fingerprints must carry the auth: namespace prefix (%q / %q)", keyA, keyB)
	}
	if nsA, nsB := semanticNamespace(keyA, "m"), semanticNamespace(keyB, "m"); nsA == "" || nsB == "" || nsA == nsB {
		t.Fatalf("namespaces must be distinct and non-empty: %q vs %q", nsA, nsB)
	}

	c.Store(keyA, "m", true, []byte(`{"resp":"a"}`), []float32{0.7071, 0.7071, 0})

	// Key B, semantically identical prompt: must miss (partitioned index).
	embedB := func() ([]float32, error) { return []float32{0.7071, 0.7071, 0}, nil }
	if _, ok, cacheType := c.Lookup(keyB, "m", true, embedB); ok {
		t.Fatalf("cross-key semantic hit (type %q) served key A's cached response", cacheType)
	}

	// Key A, same payload: exact hit.
	if data, ok, cacheType := c.Lookup(keyA, "m", true, nil); !ok || cacheType != "exact" {
		t.Fatalf("owning-key exact lookup = %v, %q; want hit/exact", data, cacheType)
	}

	// Key A, near-identical prompt: semantic hit inside its own namespace.
	similar := FingerprintPayloadWithAuth(testPayload("hello world!"), "token-a")
	embedA := func() ([]float32, error) { return []float32{0.7071, 0.7071, 0}, nil }
	if data, ok, cacheType := c.Lookup(similar, "m", true, embedA); !ok || cacheType != "semantic" {
		t.Fatalf("owning-key semantic lookup = %v, %q; want hit/semantic", data, cacheType)
	}
}

// TestResponseCache_UnscopedNamespaceIsolated verifies that legacy unscoped
// fingerprints (FingerprintPayload, tests/tooling) live in their own
// namespace and cannot reach authed entries.
func TestResponseCache_UnscopedNamespaceIsolated(t *testing.T) {
	c := NewResponseCache(16, 0, 0, 0, nil, nil, true, 0.9, okEmbedder{})
	t.Cleanup(c.Stop)

	keyA := FingerprintPayloadWithAuth(testPayload("hello world"), "token-a")
	unscoped := FingerprintPayload(testPayload("hello world"))

	c.Store(keyA, "m", true, []byte(`{"resp":"a"}`), []float32{1, 0, 0})

	embed := func() ([]float32, error) { return []float32{1, 0, 0}, nil }
	if _, ok, _ := c.Lookup(unscoped, "m", true, embed); ok {
		t.Fatal("unscoped lookup reached an authed namespace entry")
	}
}

// TestResponseCache_SemanticEmbedFailureIsAMiss covers the embedder-error
// path through the namespaced index: a different key (exact miss) whose
// embedding fails must be a clean miss.
func TestResponseCache_SemanticEmbedFailureIsAMiss(t *testing.T) {
	c := NewResponseCache(16, 0, 0, 0, nil, nil, true, 0.9, okEmbedder{})
	t.Cleanup(c.Stop)

	key := FingerprintPayloadWithAuth(testPayload("hello world"), "token-a")
	c.Store(key, "m", true, []byte(`{"resp":"a"}`), []float32{1, 0, 0})

	// Different payload (exact miss) in the same namespace; the semantic
	// fallback then fails to embed.
	other := FingerprintPayloadWithAuth(testPayload("other topic entirely"), "token-a")
	fail := func() ([]float32, error) { return nil, errors.New("embedder down") }
	if data, ok, _ := c.Lookup(other, "m", true, fail); ok {
		t.Fatalf("lookup with failing embedder must be a miss, got %q", data)
	}
}

// semanticNamespaceIndexLen reports whether the namespace index exists and
// its size, under the cache lock (a test helper for asserting pruning without
// racing the background evicter).
func (c *ResponseCache) semanticNamespaceIndexLen(namespace string) (int, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	idx, ok := c.semIdx[namespace]
	if !ok {
		return 0, false
	}
	return idx.Len(), true
}

// TestResponseCache_SemanticNamespaceEviction covers the eviction path
// through the namespaced indexes: overflow eviction removes the victim from
// its namespace index, and a namespace whose entries are all evicted is
// pruned entirely (rotated tokens must not leak indexes).
func TestResponseCache_SemanticNamespaceEviction(t *testing.T) {
	c := NewResponseCache(2, 0, 0, 0, nil, nil, true, 0.9, okEmbedder{})
	t.Cleanup(c.Stop)

	// Fill namespace A past capacity with DISTINCT vectors, so the evicted
	// entry cannot semantically match its survivor.
	keyA1 := FingerprintPayloadWithAuth(testPayload("hello one"), "token-a")
	keyA2 := FingerprintPayloadWithAuth(testPayload("hello two"), "token-a")
	keyA3 := FingerprintPayloadWithAuth(testPayload("hello three"), "token-a")
	c.Store(keyA1, "m", true, []byte(`{"r":1}`), []float32{1, 0, 0})
	c.Store(keyA2, "m", true, []byte(`{"r":2}`), []float32{0, 1, 0})
	c.Store(keyA3, "m", true, []byte(`{"r":3}`), []float32{0, 0, 1})

	// Overflow evicted the oldest namespace-A entry: a lookup with its
	// (distinct) vector is a semantic miss.
	embedA1 := func() ([]float32, error) { return []float32{1, 0, 0}, nil }
	if _, ok, _ := c.Lookup(keyA1, "m", true, embedA1); ok {
		t.Fatal("overflow-evicted entry must be a semantic miss")
	}
	if _, ok, _ := c.Lookup(keyA3, "m", true, nil); !ok {
		t.Fatal("newest namespace-A entry must survive")
	}

	// Push namespace-B entries through: namespace A drains and its index is
	// pruned instead of leaking.
	keyB1 := FingerprintPayloadWithAuth(testPayload("hello one"), "token-b")
	keyB2 := FingerprintPayloadWithAuth(testPayload("hello two"), "token-b")
	c.Store(keyB1, "m", true, []byte(`{"r":4}`), []float32{0, 1, 0})
	c.Store(keyB2, "m", true, []byte(`{"r":5}`), []float32{0, 1, 0})

	if _, ok := c.semanticNamespaceIndexLen(semanticNamespace(keyA1, "m")); ok {
		t.Fatal("drained namespace-A index was not pruned")
	}
	if _, ok := c.semanticNamespaceIndexLen(semanticNamespace(keyB1, "m")); !ok {
		t.Fatal("namespace-B index must survive while it holds entries")
	}

	// Surviving entries stay reachable within their namespace.
	embedB := func() ([]float32, error) { return []float32{0, 1, 0}, nil }
	if _, ok, cacheType := c.Lookup(FingerprintPayloadWithAuth(testPayload("hello one variant"), "token-b"), "m", true, embedB); !ok || cacheType != "semantic" {
		t.Fatalf("namespace-B semantic hit = %v, %q; want hit/semantic", ok, cacheType)
	}
}

// TestResponseCache_SemanticHitRequiresSameShape pins the round-4 guard: only
// streaming responses are stored, so a non-streaming request must not be
// served an SSE body through the semantic path (exact keys already include
// the stream field, the semantic namespace does not).
func TestResponseCache_SemanticHitRequiresSameShape(t *testing.T) {
	c := NewResponseCache(16, 0, 0, 0, nil, nil, true, 0.9, okEmbedder{})
	t.Cleanup(c.Stop)

	key := FingerprintPayloadWithAuth(testPayload("hello world"), "token-a")
	c.Store(key, "m", true, []byte("data: {...}\n\n"), []float32{1, 0, 0})

	// A near-identical prompt (exact miss) from a non-streaming request:
	// semantic candidate exists but the shape differs, so it must miss.
	nonStream := FingerprintPayloadWithAuth(testPayload("hello world!"), "token-a")
	embed := func() ([]float32, error) { return []float32{1, 0, 0}, nil }
	if data, ok, cacheType := c.Lookup(nonStream, "m", false, embed); ok {
		t.Fatalf("non-streaming lookup served response (type %q): %q", cacheType, data)
	}

	// The same near-identical prompt from a streaming request: semantic hit.
	if _, ok, cacheType := c.Lookup(nonStream, "m", true, embed); !ok || cacheType != "semantic" {
		t.Fatalf("streaming lookup = %v, %q; want semantic hit", ok, cacheType)
	}
}

// TestResponseCache_SemanticGaugeStaysExact builds one namespace past the
// per-namespace index capacity (256) and asserts the incremental gauge equals
// the true indexed count: EmbedIndex.Insert silently drops the oldest vector
// at capacity, and the accounting must follow that drop instead of drifting.
func TestResponseCache_SemanticGaugeStaysExact(t *testing.T) {
	c := NewResponseCache(400, 0, 0, 0, nil, nil, true, 0.9, okEmbedder{})
	t.Cleanup(c.Stop)

	const n = semanticIndexMaxEntries + 1
	for i := 0; i < n; i++ {
		key := FingerprintPayloadWithAuth(testPayload(fmt.Sprintf("prompt %d", i)), "token-a")
		c.Store(key, "m", true, []byte(fmt.Sprintf("resp %d", i)), []float32{1, 0, 0})
	}

	if got := c.semanticEntries; got != semanticIndexMaxEntries {
		t.Fatalf("semanticEntries = %d, want %d (index capacity)", got, semanticIndexMaxEntries)
	}
	if got := c.semanticEntryCountLocked(); got != semanticIndexMaxEntries {
		t.Fatalf("locked gauge = %d, want %d", got, semanticIndexMaxEntries)
	}

	// Re-storing an existing key must not double-count it.
	c.Store(FingerprintPayloadWithAuth(testPayload("prompt 5"), "token-a"), "m", true, []byte("resp again"), []float32{1, 0, 0})
	if got := c.semanticEntries; got != semanticIndexMaxEntries {
		t.Fatalf("semanticEntries after refresh = %d, want %d", got, semanticIndexMaxEntries)
	}
}
