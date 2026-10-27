package stream

import "testing"

// TestNormalizeUsageTotals_CacheCreationBreakdown pins the per-TTL breakdown
// (AUD-76 on the buffered path / NENYA-310): the Anthropic-native
// `cache_creation` object's ephemeral_5m/1h input tokens parse into the
// totals so every consumer (buffered conversion, usage claiming) can carry
// them. Pre-fix the sub-object was ignored and the dimensions lost.
func TestNormalizeUsageTotals_CacheCreationBreakdown(t *testing.T) {
	usage := map[string]interface{}{
		"input_tokens":                30.0,
		"output_tokens":               10.0,
		"cache_creation_input_tokens": 25.0,
		"cache_creation": map[string]interface{}{
			"ephemeral_5m_input_tokens": 20.0,
			"ephemeral_1h_input_tokens": 5.0,
		},
	}
	totals := NormalizeUsageTotals(usage)
	if totals.CacheCreation != 25 {
		t.Errorf("CacheCreation = %d, want 25", totals.CacheCreation)
	}
	if totals.CacheCreation5m != 20 {
		t.Errorf("CacheCreation5m = %d, want 20", totals.CacheCreation5m)
	}
	if totals.CacheCreation1h != 5 {
		t.Errorf("CacheCreation1h = %d, want 5", totals.CacheCreation1h)
	}

	// A partial breakdown carries only what the provider reported.
	partial := NormalizeUsageTotals(map[string]interface{}{
		"input_tokens": 1.0,
		"cache_creation": map[string]interface{}{
			"ephemeral_5m_input_tokens": 7.0,
		},
	})
	if partial.CacheCreation5m != 7 || partial.CacheCreation1h != 0 {
		t.Errorf("partial breakdown = (%d, %d), want (7, 0)", partial.CacheCreation5m, partial.CacheCreation1h)
	}

	// A malformed sub-object yields zeros, not a failure.
	bad := NormalizeUsageTotals(map[string]interface{}{
		"input_tokens":   1.0,
		"cache_creation": "garbage",
	})
	if bad.CacheCreation5m != 0 || bad.CacheCreation1h != 0 {
		t.Errorf("malformed breakdown must yield zeros, got (%d, %d)", bad.CacheCreation5m, bad.CacheCreation1h)
	}
}
