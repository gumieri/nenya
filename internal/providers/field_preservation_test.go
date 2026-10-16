package providers

import (
	"io"
	"log/slog"
	"testing"
)

// discardDeps builds SanitizeDeps with a discard logger.
func discardDeps() *SanitizeDeps {
	return &SanitizeDeps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// TestFieldPreservationClientKnobsWin pins the NENYA-151 invariant across
// the provider sanitize hooks: wherever a stage fills a DEFAULT, an
// explicit client value must win, and a stage may only strip exactly what
// its contract says.
func TestFieldPreservationClientKnobsWin(t *testing.T) {
	t.Run("zai temperature default respects client", func(t *testing.T) {
		payload := map[string]interface{}{"model": "glm-4.7", "temperature": 0.3}
		injectTemperatureDefaultsForZai(payload)
		if got := payload["temperature"]; got != 0.3 {
			t.Fatalf("client temperature rewritten: %v", got)
		}
	})

	t.Run("zai thinking auto respects client", func(t *testing.T) {
		payload := map[string]interface{}{
			"model":    "glm-4.7",
			"thinking": map[string]interface{}{"type": "disabled", "clear_thinking": true},
		}
		deps := discardDeps()
		deps.SupportsReasoning = func(string) bool { return true }
		injectThinkingForZai(deps, payload)
		thinking, ok := payload["thinking"].(map[string]interface{})
		if !ok {
			t.Fatalf("thinking must remain a map: %#v", payload["thinking"])
		}
		if thinking["type"] != "disabled" || thinking["clear_thinking"] != true {
			t.Fatalf("client thinking clobbered by auto-inject: %#v", thinking)
		}
	})

	t.Run("zai thinking force-enabled merges instead of clobbering", func(t *testing.T) {
		payload := map[string]interface{}{
			"model":    "glm-4.7",
			"thinking": map[string]interface{}{"type": "disabled", "clear_thinking": true},
		}
		deps := discardDeps()
		deps.ProviderThinking = func(string) (bool, bool, bool) { return true, false, true }
		injectThinkingForZai(deps, payload)
		thinking, ok := payload["thinking"].(map[string]interface{})
		if !ok {
			t.Fatalf("thinking must remain a map: %#v", payload["thinking"])
		}
		// The operator's force-enabled wins on the MODE (documented), but
		// the client's clear_thinking must survive.
		if thinking["type"] != "enabled" {
			t.Fatalf("force-enabled must win on the mode: %#v", thinking)
		}
		if thinking["clear_thinking"] != true {
			t.Fatalf("force-enabled must preserve the client's clear_thinking: %#v", thinking)
		}
	})

	t.Run("zai thinking force-enabled fills clear_thinking when absent", func(t *testing.T) {
		payload := map[string]interface{}{
			"model":    "glm-4.7",
			"thinking": map[string]interface{}{"type": "disabled"},
		}
		deps := discardDeps()
		deps.ProviderThinking = func(string) (bool, bool, bool) { return true, true, true }
		injectThinkingForZai(deps, payload)
		thinking, ok := payload["thinking"].(map[string]interface{})
		if !ok {
			t.Fatalf("thinking must remain a map: %#v", payload["thinking"])
		}
		if thinking["type"] != "enabled" || thinking["clear_thinking"] != true {
			t.Fatalf("force-enabled must fill missing fields from config: %#v", thinking)
		}
	})

	t.Run("gemini temperature default respects client", func(t *testing.T) {
		payload := map[string]interface{}{"model": "gemini-3-pro", "temperature": 0.2}
		injectTemperatureDefaultsForGemini(payload)
		if got := payload["temperature"]; got != 0.2 {
			t.Fatalf("client temperature rewritten: %v", got)
		}
	})

	t.Run("xai reasoning_effort default respects client", func(t *testing.T) {
		payload := map[string]interface{}{"model": "grok-4", "reasoning_effort": "low"}
		deps := discardDeps()
		deps.SupportsReasoning = func(string) bool { return true }
		XaiSanitize(deps, payload)
		if got := payload["reasoning_effort"]; got != "low" {
			t.Fatalf("client reasoning_effort rewritten: %v", got)
		}
		// The default path: an absent field is filled (the other half of
		// the contract).
		absent := map[string]interface{}{"model": "grok-4"}
		XaiSanitize(deps, absent)
		if got := absent["reasoning_effort"]; got != "medium" {
			t.Fatalf("absent reasoning_effort must get the documented default, got %v", got)
		}
	})

	t.Run("gemini maps and removes reasoning_effort", func(t *testing.T) {
		payload := map[string]interface{}{"model": "gemini-3-pro", "reasoning_effort": "high"}
		injectThinkingForGemini(discardDeps(), payload)
		if _, has := payload["reasoning_effort"]; has {
			t.Fatal("gemini contract: reasoning_effort is consumed by the mapping")
		}
		extra, ok := payload["extra_body"].(map[string]interface{})
		if !ok {
			t.Fatalf("reasoning_effort must map into extra_body.google: %#v", payload)
		}
		google, ok := extra["google"].(map[string]interface{})
		if !ok {
			t.Fatalf("missing extra_body.google: %#v", extra)
		}
		thinking, ok := google["thinking_config"].(map[string]interface{})
		if !ok || thinking == nil {
			t.Fatalf("missing thinking_config: %#v", google)
		}
		if thinking["thinkingLevel"] != "high" {
			t.Fatalf("gemini-3 maps effort to thinkingLevel, got %#v", thinking)
		}

		// Gemini 2.5 maps the same effort to a token budget instead.
		older := map[string]interface{}{"model": "gemini-2.5-pro", "reasoning_effort": "high"}
		injectThinkingForGemini(discardDeps(), older)
		google2 := older["extra_body"].(map[string]interface{})["google"].(map[string]interface{})
		thinking2, ok := google2["thinking_config"].(map[string]interface{})
		if !ok || thinking2 == nil {
			t.Fatalf("missing thinking_config for the budget generation: %#v", google2)
		}
		if _, has := thinking2["thinkingBudget"]; !has {
			t.Fatalf("gemini-2.5 maps effort to thinkingBudget, got %#v", thinking2)
		}
	})
}
