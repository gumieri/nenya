package routing

import (
	"io"
	"log/slog"
	"testing"
)

// TestFieldPreservationStripContracts pins the ONLY allowed strips in the
// sanitize stage: each is a documented contract, scoped exactly to its
// trigger (NENYA-151).
func TestFieldPreservationStripContracts(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("thinking-mode strip is scoped to enabled thinking", func(t *testing.T) {
		enabled := map[string]interface{}{
			"model":       "deepseek-v4",
			"temperature": 0.3,
			"top_p":       0.9,
			"thinking":    map[string]interface{}{"type": "enabled"},
		}
		stripDeepSeekThinkingParams(enabled, "deepseek-v4", logger)
		if _, has := enabled["temperature"]; has {
			t.Fatal("enabled thinking must strip the sampling params (documented contract)")
		}

		disabled := map[string]interface{}{
			"model":       "deepseek-v4",
			"temperature": 0.3,
			"thinking":    map[string]interface{}{"type": "disabled"},
		}
		stripDeepSeekThinkingParams(disabled, "deepseek-v4", logger)
		if got := disabled["temperature"]; got != 0.3 {
			t.Fatalf("disabled thinking must keep the client's temperature, got %v", got)
		}
	})

	t.Run("bare reasoning strip respects an explicit reasoning_effort twin", func(t *testing.T) {
		payload := map[string]interface{}{
			"model":            "deepseek-v4",
			"reasoning":        map[string]interface{}{"effort": "high"},
			"reasoning_effort": "low",
		}
		deps := TransformDeps{Logger: logger}
		stripBareReasoningField(deps, payload, "deepseek-v4")
		if _, has := payload["reasoning"]; has {
			t.Fatal("the redundant reasoning twin must be stripped (contract)")
		}
		if got := payload["reasoning_effort"]; got != "low" {
			t.Fatalf("explicit reasoning_effort rewritten: %v", got)
		}
	})

	t.Run("deepseek history injection leaves request knobs alone", func(t *testing.T) {
		payload := map[string]interface{}{
			"model": "deepseek-v4",
			"messages": []interface{}{
				map[string]interface{}{"role": "assistant", "content": "earlier"},
			},
			"reasoning_effort": "low",
			"temperature":      0.1,
		}
		ensureDeepSeekReasoningContent(payload, logger)
		if got := payload["reasoning_effort"]; got != "low" {
			t.Fatalf("reasoning_effort rewritten: %v", got)
		}
		if got := payload["temperature"]; got != 0.1 {
			t.Fatalf("temperature rewritten: %v", got)
		}
	})
}
