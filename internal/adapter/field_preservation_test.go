package adapter

import (
	"strings"
	"testing"
)

// TestFieldPreservationAdapterConversions pins the conversion-pair half of
// the NENYA-151 invariant: a client-set sampling knob crosses
// OpenAI→Anthropic verbatim (or maps to the documented target-equivalent),
// and the reasoning_effort→thinking mapping never overwrites a client-set
// thinking config.
func TestFieldPreservationAdapterConversions(t *testing.T) {
	a := NewAnthropicAdapter()
	openai := map[string]interface{}{
		"model":            "claude-sonnet-4",
		"max_tokens":       float64(1024),
		"temperature":      0.42,
		"top_p":            0.9,
		"stop":             []interface{}{"END"},
		"reasoning_effort": "high",
		"thinking":         map[string]interface{}{"type": "enabled", "budget_tokens": 2048},
	}
	converted := a.ConvertOpenAIToAnthropicBody(openai, "claude-sonnet-4", false, CacheOpts{})

	if got := converted["temperature"]; got != 0.42 {
		t.Fatalf("temperature not preserved: %v", got)
	}
	if got := converted["top_p"]; got != 0.9 {
		t.Fatalf("top_p not preserved: %v", got)
	}
	if got := converted["max_tokens"]; got != 1024 {
		t.Fatalf("max_tokens not preserved: %v", got)
	}
	if _, ok := converted["stop_sequences"]; !ok {
		t.Fatal("stop must map to stop_sequences")
	}
	thinking, ok := converted["thinking"].(map[string]interface{})
	if !ok {
		t.Fatalf("the client's thinking config must survive conversion: %#v", converted["thinking"])
	}
	if thinking["budget_tokens"] != 2048 {
		t.Fatalf("the client's thinking config must win over the reasoning_effort mapping: %#v", thinking)
	}
	// Anthropic-shaped only: a zai-shaped map (the shape the sanitize hooks
	// inject into the shared failover payload) must be dropped, not carried
	// into a guaranteed-400 body.
	zai := a.ConvertOpenAIToAnthropicBody(map[string]interface{}{
		"model":    "claude-sonnet-4",
		"thinking": map[string]interface{}{"type": "enabled", "clear_thinking": false},
	}, "claude-sonnet-4", false, CacheOpts{})
	if _, carried := zai["thinking"]; carried {
		t.Fatalf("a non-Anthropic-shaped thinking map must not be carried: %#v", zai["thinking"])
	}

	// Copied, not aliased: mutating the converted body must not touch the
	// caller's payload (reused across failover targets).
	thinking["budget_tokens"] = 999999
	if openai["thinking"].(map[string]interface{})["budget_tokens"] != 2048 {
		t.Fatal("the conversion aliased the caller's thinking map")
	}
	_ = strings.TrimSpace
}
