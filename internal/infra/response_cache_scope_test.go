package infra

import (
	"strings"
	"testing"
)

// TestFingerprintPayloadScoped_Deterministic pins the NENYA-154 contract that
// a fingerprint is a pure function of (payload, auth, policy scope): identical
// inputs must keep sharing entries, or every request would be a cache miss.
func TestFingerprintPayloadScoped_Deterministic(t *testing.T) {
	payload := testPayload("hello world")
	policy := `{"agent":"chat","exfil_action":"strip"}`

	first := FingerprintPayloadScoped(payload, "token-a", policy)
	second := FingerprintPayloadScoped(testPayload("hello world"), "token-a", policy)
	if first == "" {
		t.Fatal("fingerprint returned empty string")
	}
	if first != second {
		t.Fatalf("identical inputs produced different fingerprints: %q != %q", first, second)
	}
	if !strings.HasPrefix(first, "auth:") {
		t.Fatalf("auth-scoped fingerprint must carry the auth: namespace prefix, got %q", first)
	}
}

// TestFingerprintPayloadScoped_PolicyChangesKey is the regression guard for the
// GoModel #971 class: a body cached under one content policy must never be
// reachable once the policy changes.
func TestFingerprintPayloadScoped_PolicyChangesKey(t *testing.T) {
	payload := testPayload("hello world")

	permissive := FingerprintPayloadScoped(payload, "token-a", `{"exfil_action":"log"}`)
	strict := FingerprintPayloadScoped(payload, "token-a", `{"exfil_action":"strip"}`)
	if permissive == strict {
		t.Fatal("policy change must change the fingerprint")
	}

	// The auth partition stays independent of the policy partition.
	otherAuth := FingerprintPayloadScoped(payload, "token-b", `{"exfil_action":"log"}`)
	if otherAuth == permissive {
		t.Fatal("auth change must change the fingerprint")
	}
}

// TestFingerprintPayloadScoped_EmptyPolicyMatchesAuthOnly pins the helper
// relationship: the two-argument helper and the three-argument helper with an
// empty scope are the same key. It does NOT promise byte-compatibility with
// pre-NENYA-154 keys — folding the (empty) scope digest into the namespace
// segment deliberately changes the namespace format.
func TestFingerprintPayloadScoped_EmptyPolicyMatchesAuthOnly(t *testing.T) {
	payload := testPayload("hello world")
	if got, want := FingerprintPayloadWithAuth(payload, "token-a"), FingerprintPayloadScoped(payload, "token-a", ""); got != want {
		t.Fatalf("empty-policy fingerprint = %q, want %q", got, want)
	}
	// The unscoped form cannot be partitioned by policy, so a scoped request
	// without a caller identity fails closed (no cacheable key).
	if got := FingerprintPayloadScoped(payload, "", `{"agent":"chat"}`); got != "" {
		t.Fatalf("scoped fingerprint without a token must be empty, got %q", got)
	}
	if got, want := FingerprintPayload(payload), FingerprintPayloadScoped(payload, "", ""); got != want {
		t.Fatalf("unscoped fingerprint drift: %q != %q", got, want)
	}
}

// TestFingerprintPayloadScoped_NoDelimiterCollision pins that scoping hashes
// fixed-width digests rather than a token+scope concatenation: inputs that a
// naive `token + scope` join would collide on must still produce distinct keys
// (the hashes of the two fields are fed as separate 32-byte values).
func TestFingerprintPayloadScoped_NoDelimiterCollision(t *testing.T) {
	payload := testPayload("hello world")
	a := FingerprintPayloadScoped(payload, "a", "bc")
	b := FingerprintPayloadScoped(payload, "ab", "c")
	if a == b {
		t.Fatal("token/scope boundary is ambiguous: distinct inputs collided")
	}
}

// TestResponseCache_SemanticPartitionedByPolicy covers the semantic path: a
// near-identical prompt must not reach an entry stored under a different
// content-policy scope, even for the same API key and model.
func TestResponseCache_SemanticPartitionedByPolicy(t *testing.T) {
	c := NewResponseCache(16, 0, 0, 0, nil, nil, true, 0.9, okEmbedder{})
	t.Cleanup(c.Stop)

	policyA := `{"exfil_action":"log"}`
	policyB := `{"exfil_action":"strip"}`
	keyA := FingerprintPayloadScoped(testPayload("hello world"), "token-a", policyA)
	keyB := FingerprintPayloadScoped(testPayload("hello world!"), "token-a", policyB)
	if nsA, nsB := semanticNamespace(keyA, "m"), semanticNamespace(keyB, "m"); nsA == "" || nsA == nsB {
		t.Fatalf("policy scopes must yield distinct namespaces: %q vs %q", nsA, nsB)
	}

	c.Store(keyA, "m", true, []byte(`{"resp":"a"}`), []float32{0.7071, 0.7071, 0})

	embed := func() ([]float32, error) { return []float32{0.7071, 0.7071, 0}, nil }
	if data, ok, cacheType := c.Lookup(keyB, "m", true, embed); ok {
		t.Fatalf("policy-B lookup reached policy-A entry (type %q): %q", cacheType, data)
	}

	// Same policy, near-identical prompt: the semantic fallback still works.
	samePolicy := FingerprintPayloadScoped(testPayload("hello world!"), "token-a", policyA)
	if _, ok, cacheType := c.Lookup(samePolicy, "m", true, embed); !ok || cacheType != "semantic" {
		t.Fatalf("same-policy semantic lookup = %v, %q; want semantic hit", ok, cacheType)
	}
}
