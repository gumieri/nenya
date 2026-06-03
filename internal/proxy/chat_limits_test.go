package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.0ur.uk/nenya/config"
	"git.0ur.uk/nenya/internal/gateway"
	"git.0ur.uk/nenya/internal/routing"
	"git.0ur.uk/nenya/internal/testutil"
)

func TestResolvePipelineContext_UnknownMaxContext(t *testing.T) {
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
	secrets := &config.SecretsConfig{
		ClientToken: "test-token",
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())

	proxy := &Proxy{}

	t.Run("MaxContext=0 returns zero limits and logs warning", func(t *testing.T) {
		req := &chatRequest{
			ModelName: "qwen3:14b",
			Payload: map[string]any{
				"model":    "qwen3:14b",
				"messages": []any{map[string]any{"role": "user", "content": "test"}},
			},
			Targets: []routing.UpstreamTarget{
				{
					Provider:   "ollama",
					Model:      "qwen3:14b",
					MaxContext: 0,
				},
			},
		}

		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

		messages, _, softLimit, hardLimit, _, _ := proxy.resolvePipelineContext(r, gw, req)

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
		req := &chatRequest{
			ModelName: "qwen3:14b",
			Payload: map[string]any{
				"model":    "qwen3:14b",
				"messages": []any{map[string]any{"role": "user", "content": "test"}},
			},
			Targets: []routing.UpstreamTarget{
				{
					Provider:   "ollama",
					Model:      "qwen3:14b",
					MaxContext: 128000,
				},
			},
		}

		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

		messages, _, softLimit, hardLimit, _, _ := proxy.resolvePipelineContext(r, gw, req)

		expectedSoft := 128000 / 8
		expectedHard := 128000 * 3 / 4

		if softLimit != expectedSoft {
			t.Errorf("expected softLimit=%d, got %d", expectedSoft, softLimit)
		}
		if hardLimit != expectedHard {
			t.Errorf("expected hardLimit=%d, got %d", expectedHard, hardLimit)
		}
		if messages == nil {
			t.Error("expected non-nil messages")
		}
	})

	t.Run("Empty messages returns zero limits", func(t *testing.T) {
		req := &chatRequest{
			ModelName: "qwen3:14b",
			Payload: map[string]any{
				"model":    "qwen3:14b",
				"messages": []any{},
			},
			Targets: []routing.UpstreamTarget{
				{
					Provider:   "ollama",
					Model:      "qwen3:14b",
					MaxContext: 128000,
				},
			},
		}

		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

		_, _, softLimit, hardLimit, _, _ := proxy.resolvePipelineContext(r, gw, req)

		if softLimit != 0 {
			t.Errorf("expected softLimit=0 for empty messages, got %d", softLimit)
		}
		if hardLimit != 0 {
			t.Errorf("expected hardLimit=0 for empty messages, got %d", hardLimit)
		}
	})
}
