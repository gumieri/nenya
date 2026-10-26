package routing

import "testing"

// TestApplyMaxTokensClampKeepsFloat64 pins AUD-36 (NENYA-247): the clamp
// branch must store the canonical JSON numeric type (float64, NENYA-186),
// exactly like the set branch. Pre-fix it stored a Go int, so a LATER clamp
// — a smaller-cap failover target — hit the float64 type assertion with !ok
// and silently kept the larger cap: the type contract's stated purpose
// ("later comparisons never no-op on a type mismatch") was broken by the
// clamp's own store.
func TestApplyMaxTokensClampKeepsFloat64(t *testing.T) {
	payload := map[string]interface{}{
		"max_tokens": float64(5000),
	}

	applyMaxTokens(payload, 1000) // first clamp: 5000 -> 1000
	if v, ok := payload["max_tokens"].(float64); !ok || v != 1000 {
		t.Fatalf("after the first clamp max_tokens = %T %v, want float64 1000", payload["max_tokens"], payload["max_tokens"])
	}

	applyMaxTokens(payload, 500) // second clamp: a smaller-cap target
	if v, ok := payload["max_tokens"].(float64); !ok || v != 500 {
		t.Fatalf("after the second clamp max_tokens = %T %v, want float64 500 (a type-mismatched store made the later clamp no-op)", payload["max_tokens"], payload["max_tokens"])
	}
}

// TestApplyMaxTokensSetStoresFloat64 pins the set branch for completeness:
// an absent cap is stored as float64 too.
func TestApplyMaxTokensSetStoresFloat64(t *testing.T) {
	payload := map[string]interface{}{}
	applyMaxTokens(payload, 777)
	if v, ok := payload["max_tokens"].(float64); !ok || v != 777 {
		t.Fatalf("max_tokens = %T %v, want float64 777", payload["max_tokens"], payload["max_tokens"])
	}
}
