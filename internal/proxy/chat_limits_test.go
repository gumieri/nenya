package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/testutil"
)

func TestResolvePipelineContext_UnknownMaxContext(t *testing.T) {
	proxy := &Proxy{}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	t.Run("MaxContext=0 returns zero limits and disables truncation", func(t *testing.T) {
		gw := newLimitsTestGateway(t)
		messages, _, softLimit, hardLimit, _, _ := proxy.resolvePipelineContext(r, gw, limitsRequest(0, 0))
		if softLimit != 0 {
			t.Errorf("expected softLimit=0, got %d", softLimit)
		}
		if hardLimit != 0 {
			t.Errorf("expected hardLimit=0, got %d", hardLimit)
		}
		if messages == nil {
			t.Error("expected non-nil messages")
		}
	})

	t.Run("MaxContext>0 returns calculated limits", func(t *testing.T) {
		// MaxOutput is unset here, so this exercises the 3/4 fallback of the
		// output-aware derivation (see the output-aware test for reservation).
		gw := newLimitsTestGateway(t)
		messages, _, softLimit, hardLimit, _, _ := proxy.resolvePipelineContext(r, gw, limitsRequest(128000, 0))
		wantSoft := 128000 / 8
		wantHard := 128000 / 4 * 3
		if softLimit != wantSoft {
			t.Errorf("expected softLimit=%d, got %d", wantSoft, softLimit)
		}
		if hardLimit != wantHard {
			t.Errorf("expected hardLimit=%d, got %d", wantHard, hardLimit)
		}
		if messages == nil {
			t.Error("expected non-nil messages")
		}
	})

	t.Run("Empty messages returns zero limits", func(t *testing.T) {
		gw := newLimitsTestGateway(t)
		req := limitsRequest(128000, 0)
		req.Payload["messages"] = []any{}
		_, _, softLimit, hardLimit, _, _ := proxy.resolvePipelineContext(r, gw, req)
		if softLimit != 0 {
			t.Errorf("expected softLimit=0 for empty messages, got %d", softLimit)
		}
		if hardLimit != 0 {
			t.Errorf("expected hardLimit=0 for empty messages, got %d", hardLimit)
		}
	})
}

// newLimitsTestGateway builds the minimal gateway used by the limit tests.
func newLimitsTestGateway(t *testing.T) *gateway.NenyaGateway {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Providers = map[string]config.ProviderConfig{
		"ollama": {
			URL:       "http://localhost:11434/v1/chat/completions",
			AuthStyle: "none",
		},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models: []config.AgentModel{
				{Provider: "ollama", Model: "qwen3:14b"},
			},
		},
	}
	secrets := &config.SecretsConfig{ClientToken: "test-token"}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	t.Cleanup(gw.Close)
	return gw
}

func limitsRequest(maxContext, maxOutput int) *chatRequest {
	return &chatRequest{
		ModelName: "qwen3:14b",
		Payload: map[string]any{
			"model":    "qwen3:14b",
			"messages": []any{map[string]any{"role": "user", "content": "test"}},
		},
		Targets: []routing.UpstreamTarget{
			{Provider: "ollama", Model: "qwen3:14b", MaxContext: maxContext, MaxOutput: maxOutput},
		},
	}
}

// TestResolvePipelineContext_OutputAwareHardLimit pins Phase 012: the hard limit
// reserves the model's output room instead of always using 3/4 of the context
// window.
func TestResolvePipelineContext_OutputAwareHardLimit(t *testing.T) {
	gw := newLimitsTestGateway(t)
	proxy := &Proxy{}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	tests := []struct {
		name       string
		maxContext int
		maxOutput  int
		wantSoft   int
		wantHard   int
	}{
		// 1M ctx / 384K out: 3/4 = 750K would allow 750K + 384K > 1M, so the
		// output reservation (1M - 384K = 616K) wins.
		{"overshoot regression", 1_000_000, 384_000, 125_000, 616_000},
		// Output unknown: 3/4 fallback, unchanged behavior.
		{"unknown output keeps three quarters", 128_000, 0, 16_000, 96_000},
		// Misconfigured output >= context: subtracting would disable trimming,
		// so the 3/4 budget is kept.
		{"output at least context falls back", 128_000, 200_000, 16_000, 96_000},
		// Reservation larger than 3/4: 3/4 wins.
		{"three quarters wins when larger", 1_000_000, 100_000, 125_000, 750_000},
		// Tiny window with a positive output cap: reservation (10-8=2) wins
		// over 3/4 (6), exercising the small-value guard.
		{"tiny window with output", 10, 8, 1, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, soft, hard, _, _ := proxy.resolvePipelineContext(r, gw, limitsRequest(tt.maxContext, tt.maxOutput))
			if soft != tt.wantSoft {
				t.Errorf("softLimit = %d, want %d", soft, tt.wantSoft)
			}
			if hard != tt.wantHard {
				t.Errorf("hardLimit = %d, want %d", hard, tt.wantHard)
			}
		})
	}
}

// TestResolvePipelineContext_ReservesEffectiveOutput verifies the reservation
// uses the request's effective output room: a client-supplied max_tokens below
// the model cap must not trigger the worst-case declared-cap reservation, which
// would over-trim. hard_limit_tokens still wins outright.
func TestResolvePipelineContext_ReservesEffectiveOutput(t *testing.T) {
	gw := newLimitsTestGateway(t)
	proxy := &Proxy{}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	t.Run("client max_tokens below cap avoids over-reservation", func(t *testing.T) {
		req := limitsRequest(1_000_000, 384_000)
		req.Payload["max_tokens"] = float64(8192)
		_, _, _, hard, _, _ := proxy.resolvePipelineContext(r, gw, req)
		// min(3/4, 1M-8192) = 750K: no over-trim for a small output request.
		if hard != 750_000 {
			t.Errorf("hardLimit = %d, want 750000", hard)
		}
	})

	t.Run("client max_tokens above cap uses the cap", func(t *testing.T) {
		req := limitsRequest(1_000_000, 384_000)
		req.Payload["max_tokens"] = float64(900_000)
		_, _, _, hard, _, _ := proxy.resolvePipelineContext(r, gw, req)
		if hard != 616_000 {
			t.Errorf("hardLimit = %d, want 616000", hard)
		}
	})

	t.Run("hard_limit_tokens overrides the derived limit", func(t *testing.T) {
		gw.Config.Context.HardLimitTokens = 500_000
		t.Cleanup(func() { gw.Config.Context.HardLimitTokens = 0 })
		_, _, _, hard, _, _ := proxy.resolvePipelineContext(r, gw, limitsRequest(1_000_000, 384_000))
		if hard != 500_000 {
			t.Errorf("hardLimit = %d, want override 500000", hard)
		}
	})
}

// TestResolvePipelineContext_SoftLimitOverride pins the context.soft_limit_tokens
// override: when positive it replaces the MaxContext/8 derivation (clamped to
// the window); when unset the derived default is kept.
func TestResolvePipelineContext_SoftLimitOverride(t *testing.T) {
	proxy := &Proxy{}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	t.Run("override replaces derived soft limit", func(t *testing.T) {
		gw := newLimitsTestGateway(t)
		gw.Config.Context.SoftLimitTokens = 5000
		_, _, soft, hard, _, _ := proxy.resolvePipelineContext(r, gw, limitsRequest(128_000, 0))
		if soft != 5000 {
			t.Errorf("softLimit = %d, want override 5000", soft)
		}
		if hard != 96_000 {
			t.Errorf("hardLimit = %d, want 96000 (override must not touch hard limit)", hard)
		}
	})

	t.Run("override clamped to the hard limit", func(t *testing.T) {
		gw := newLimitsTestGateway(t)
		gw.Config.Context.SoftLimitTokens = 500_000
		_, _, soft, hard, _, _ := proxy.resolvePipelineContext(r, gw, limitsRequest(128_000, 0))
		if soft != hard {
			t.Errorf("softLimit = %d, want clamp to hardLimit %d", soft, hard)
		}
		if soft != 96_000 {
			t.Errorf("softLimit = %d, want 96000", soft)
		}
	})

	t.Run("unset keeps derived soft limit", func(t *testing.T) {
		gw := newLimitsTestGateway(t)
		_, _, soft, _, _, _ := proxy.resolvePipelineContext(r, gw, limitsRequest(128_000, 0))
		if soft != 16_000 {
			t.Errorf("softLimit = %d, want derived 16000", soft)
		}
	})
}

// (util.DeriveInputTokenBudget is covered in internal/util/math_test.go.)
