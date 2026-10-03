package proxy

import (
	"context"
	"testing"

	"github.com/nenya/config"
)

// TestCacheAwareActive pins the Phase 015 gating matrix: the policy is active
// only for an opted-in agent, under the hard limit, and (for "auto") on a
// provider that caches prefixes automatically.
func TestCacheAwareActive(t *testing.T) {
	force := &config.AgentConfig{CacheAware: config.CacheAwareForce}
	auto := &config.AgentConfig{CacheAware: config.CacheAwareAuto}
	off := &config.AgentConfig{CacheAware: config.CacheAwareOff}
	unset := &config.AgentConfig{}

	tests := []struct {
		name         string
		agent        *config.AgentConfig
		provider     string
		tokenCount   int
		hardLimit    int
		windowMaxCtx int
		want         bool
	}{
		{"nil agent", nil, "deepseek", 100, 1000, 0, false},
		{"off", off, "deepseek", 100, 1000, 0, false},
		{"unset", unset, "deepseek", 100, 1000, 0, false},
		{"auto cache-rich under limit", auto, "deepseek", 100, 1000, 0, true},
		{"auto cache-rich below limit by one", auto, "deepseek", 999, 1000, 0, true},
		{"auto cache-rich at limit", auto, "deepseek", 1000, 1000, 0, false},
		{"auto cache-rich over limit", auto, "deepseek", 2000, 1000, 0, false},
		{"auto anthropic", auto, "anthropic", 100, 1000, 0, true},
		{"auto openai", auto, "openai", 100, 1000, 0, true},
		{"auto cache-poor provider", auto, "openrouter", 100, 1000, 0, false},
		{"auto unknown provider", auto, "nope", 100, 1000, 0, false},
		{"force cache-poor provider", force, "openrouter", 100, 1000, 0, true},
		{"force over limit", force, "deepseek", 2000, 1000, 0, false},
		{"auto unknown hard limit no window guard", auto, "deepseek", 999999, 0, 0, true},
		{"force unknown hard limit no window guard", force, "openrouter", 999999, 0, 0, true},
		{"auto unknown hard limit with window guard", auto, "deepseek", 999999, 0, 128000, false},
		{"force unknown hard limit with window guard", force, "openrouter", 999999, 0, 128000, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cacheAwareActive(tt.agent, tt.provider, tt.tokenCount, tt.hardLimit, tt.windowMaxCtx); got != tt.want {
				t.Errorf("cacheAwareActive = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestApplyWindowStage_SkippedWhenCacheAware proves the window stage returns
// before touching the gateway when cache_aware is active (a nil gateway would
// otherwise panic in buildWindowDeps).
func TestApplyWindowStage_SkippedWhenCacheAware(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": "x"}}
	payload := map[string]any{"messages": msgs}
	// Must not panic: the early return precedes buildWindowDeps.
	applyWindowStage(nil, context.Background(), contentPipelineOpts{}, payload, msgs, 10, true)
	if len(payload["messages"].([]any)) != 1 {
		t.Errorf("payload mutated by a skipped window stage")
	}
}
