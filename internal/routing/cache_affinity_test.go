package routing

import (
	"testing"

	"github.com/nenya/config"
)

// TestPromptCacheKeyPerConversation pins NENYA-158: the injected
// prompt_cache_key derives from the coherent session identity — a warm turn
// carries the same key as turn 1, and distinct conversations carry distinct
// keys — while a client-supplied key still wins.
func TestPromptCacheKeyPerConversation(t *testing.T) {
	deps := func(sessionIdentity string) TransformDeps {
		cfg := &config.Config{}
		cfg.PrefixCache.Enabled = true
		return TransformDeps{Config: cfg, SessionIdentity: sessionIdentity, AgentName: "agent-a"}
	}

	turn1 := map[string]interface{}{}
	injectPromptCacheKey(deps("nenya-aaaa"), turn1, "openai", "gpt-4")
	turn2 := map[string]interface{}{}
	injectPromptCacheKey(deps("nenya-aaaa"), turn2, "openai", "gpt-4")
	if turn1["prompt_cache_key"] != turn2["prompt_cache_key"] {
		t.Fatalf("warm turns must share the per-conversation key: %v vs %v", turn1["prompt_cache_key"], turn2["prompt_cache_key"])
	}
	other := map[string]interface{}{}
	injectPromptCacheKey(deps("nenya-bbbb"), other, "openai", "gpt-4")
	if other["prompt_cache_key"] == turn1["prompt_cache_key"] {
		t.Fatalf("distinct conversations must carry distinct keys: %v", other["prompt_cache_key"])
	}

	// The per-agent fallback still applies without a session identity.
	fallback := map[string]interface{}{}
	injectPromptCacheKey(TransformDeps{Config: func() *config.Config {
		c := &config.Config{}
		c.PrefixCache.Enabled = true
		return c
	}(), AgentName: "agent-a"}, fallback, "openai", "gpt-4")
	if fallback["prompt_cache_key"] == "" {
		t.Fatal("the agent:model fallback must still inject")
	}

	// Client-supplied wins.
	client := map[string]interface{}{"prompt_cache_key": "client-key"}
	injectPromptCacheKey(deps("nenya-aaaa"), client, "openai", "gpt-4")
	if client["prompt_cache_key"] != "client-key" {
		t.Fatalf("a client-supplied key must win: %v", client["prompt_cache_key"])
	}
}

// TestPromptCacheKeyHeaderConventionSuppressesBody pins the knob's
// directionality: cache_affinity=header suppresses the body-field injection
// (the identity rides the Session-Id header instead), while body mode
// injects on any provider regardless of its name.
func TestPromptCacheKeyHeaderConventionSuppressesBody(t *testing.T) {
	cfg := &config.Config{}
	cfg.PrefixCache.Enabled = true

	header := TransformDeps{Config: cfg, SessionIdentity: "nenya-aaaa", AgentName: "agent-a", CacheAffinity: "header"}
	payload := map[string]interface{}{}
	injectPromptCacheKey(header, payload, "openai", "gpt-4")
	if _, has := payload["prompt_cache_key"]; has {
		t.Fatalf("header convention must suppress the body field: %#v", payload)
	}

	body := TransformDeps{Config: cfg, SessionIdentity: "nenya-aaaa", AgentName: "agent-a", CacheAffinity: "body"}
	payload2 := map[string]interface{}{}
	injectPromptCacheKey(body, payload2, "some-other-provider", "gpt-4")
	if payload2["prompt_cache_key"] == "" {
		t.Fatal("explicit body mode must inject regardless of the provider name")
	}
}
