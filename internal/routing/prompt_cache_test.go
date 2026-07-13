package routing

import (
	"encoding/hex"
	"testing"

	"github.com/nenya/config"
)

func defaultInjectDeps() TransformDeps {
	return TransformDeps{
		Config: &config.Config{
			PrefixCache: config.PrefixCacheConfig{
				Enabled: true,
			},
		},
		AgentName: "test-agent",
	}
}

func TestInjectPromptCacheKey_InjectsForXAI(t *testing.T) {
	deps := defaultInjectDeps()
	payload := map[string]interface{}{
		"model": "grok-2",
	}
	injectPromptCacheKey(deps, payload, "xai", "grok-2")

	key, ok := payload["prompt_cache_key"].(string)
	if !ok {
		t.Fatal("expected prompt_cache_key to be injected")
	}
	if len(key) != 16 {
		t.Fatalf("expected key to be 16 chars, got %d: %q", len(key), key)
	}
	if _, err := hex.DecodeString(key); err != nil {
		t.Fatalf("expected key to be valid hex, got %q: %v", key, err)
	}
}

func TestInjectPromptCacheKey_InjectsForOpenAI(t *testing.T) {
	deps := defaultInjectDeps()
	payload := map[string]interface{}{
		"model": "gpt-4.1",
	}
	injectPromptCacheKey(deps, payload, "openai", "gpt-4.1")

	key, ok := payload["prompt_cache_key"].(string)
	if !ok {
		t.Fatal("expected prompt_cache_key to be injected")
	}
	if len(key) != 16 {
		t.Fatalf("expected key to be 16 chars, got %d: %q", len(key), key)
	}
	if _, err := hex.DecodeString(key); err != nil {
		t.Fatalf("expected key to be valid hex, got %q: %v", key, err)
	}
}

func TestInjectPromptCacheKey_SkipsOtherProviders(t *testing.T) {
	deps := defaultInjectDeps()
	payload := map[string]interface{}{
		"model": "claude-sonnet-5",
	}
	injectPromptCacheKey(deps, payload, "anthropic", "claude-sonnet-5")

	if _, has := payload["prompt_cache_key"]; has {
		t.Error("expected prompt_cache_key NOT to be injected for anthropic")
	}
}

func TestInjectPromptCacheKey_DisabledWhenCacheOff(t *testing.T) {
	deps := defaultInjectDeps()
	deps.Config.PrefixCache.Enabled = false
	payload := map[string]interface{}{
		"model": "grok-2",
	}
	injectPromptCacheKey(deps, payload, "xai", "grok-2")

	if _, has := payload["prompt_cache_key"]; has {
		t.Error("expected prompt_cache_key NOT to be injected when cache disabled")
	}
}

func TestInjectPromptCacheKey_StableKeyPerAgentModel(t *testing.T) {
	deps := defaultInjectDeps()
	payload1 := map[string]interface{}{"model": "grok-2"}
	payload2 := map[string]interface{}{"model": "grok-2"}
	injectPromptCacheKey(deps, payload1, "xai", "grok-2")
	injectPromptCacheKey(deps, payload2, "xai", "grok-2")

	key1 := payload1["prompt_cache_key"].(string)
	key2 := payload2["prompt_cache_key"].(string)
	if key1 != key2 {
		t.Errorf("expected stable key for same agent+model, got %q vs %q", key1, key2)
	}
	if len(key1) != 16 {
		t.Fatalf("expected key to be 16 chars, got %d: %q", len(key1), key1)
	}
	if _, err := hex.DecodeString(key1); err != nil {
		t.Fatalf("expected key to be valid hex, got %q: %v", key1, err)
	}
}

func TestInjectPromptCacheKey_DifferentAgentsDifferentKeys(t *testing.T) {
	depsA := defaultInjectDeps()
	depsA.AgentName = "agent-a"
	depsB := defaultInjectDeps()
	depsB.AgentName = "agent-b"

	payloadA := map[string]interface{}{"model": "grok-2"}
	payloadB := map[string]interface{}{"model": "grok-2"}
	injectPromptCacheKey(depsA, payloadA, "xai", "grok-2")
	injectPromptCacheKey(depsB, payloadB, "xai", "grok-2")

	keyA := payloadA["prompt_cache_key"].(string)
	keyB := payloadB["prompt_cache_key"].(string)
	if keyA == keyB {
		t.Error("expected different keys for different agents")
	}
}

func TestInjectPromptCacheKey_SkipsIfAlreadyPresent(t *testing.T) {
	deps := defaultInjectDeps()
	payload := map[string]interface{}{
		"model":            "grok-2",
		"prompt_cache_key": "custom-key",
	}
	injectPromptCacheKey(deps, payload, "xai", "grok-2")

	key, ok := payload["prompt_cache_key"].(string)
	if !ok || key != "custom-key" {
		t.Errorf("expected custom key to be preserved, got %q", key)
	}
}

func TestInjectPromptCacheKey_NilConfig(t *testing.T) {
	deps := TransformDeps{
		Config:     nil,
		AgentName:  "test",
	}
	payload := map[string]interface{}{"model": "grok-2"}
	injectPromptCacheKey(deps, payload, "xai", "grok-2")

	if _, has := payload["prompt_cache_key"]; has {
		t.Error("expected prompt_cache_key NOT to be injected when config is nil")
	}
}

func TestInjectPromptCacheKey_SkipsGemini(t *testing.T) {
	deps := defaultInjectDeps()
	payload := map[string]interface{}{
		"model": "gemini-2.0-flash",
	}
	injectPromptCacheKey(deps, payload, "gemini", "gemini-2.0-flash")

	if _, has := payload["prompt_cache_key"]; has {
		t.Error("expected prompt_cache_key NOT to be injected for gemini")
	}
}

func TestInjectPromptCacheKey_EmptyAgentName(t *testing.T) {
	deps := defaultInjectDeps()
	deps.AgentName = ""
	payload := map[string]interface{}{
		"model": "grok-2",
	}
	injectPromptCacheKey(deps, payload, "xai", "grok-2")

	if _, has := payload["prompt_cache_key"]; has {
		t.Error("expected prompt_cache_key NOT to be injected when agent name is empty")
	}
}
