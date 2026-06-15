package pipeline

import (
	"log/slog"
	"os"
	"testing"

	"github.com/nenya/config"
)

func TestTrimPayload_ZeroLimit(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "You are a helpful assistant."},
			map[string]interface{}{"role": "user", "content": "Hello world"},
			map[string]interface{}{"role": "assistant", "content": "Hi there!"},
		},
	}

	countTokens := func(s string) int {
		return len(s)
	}

	cfg := config.ContextConfig{}

	t.Run("maxTokens=0 returns no-op", func(t *testing.T) {
		modified, saved := TrimPayload(logger, payload, 0, countTokens, cfg)

		if modified {
			t.Error("expected no modification with maxTokens=0")
		}
		if saved != 0 {
			t.Errorf("expected saved=0, got %d", saved)
		}

		messages, ok := payload["messages"].([]interface{})
		if !ok || len(messages) != 3 {
			t.Error("expected 3 messages to remain unchanged")
		}
	})

	t.Run("maxTokens=-1 returns no-op", func(t *testing.T) {
		modified, saved := TrimPayload(logger, payload, -1, countTokens, cfg)

		if modified {
			t.Error("expected no modification with maxTokens=-1")
		}
		if saved != 0 {
			t.Errorf("expected saved=0, got %d", saved)
		}
	})

	t.Run("payload under limit returns no-op", func(t *testing.T) {
		modified, saved := TrimPayload(logger, payload, 1000, countTokens, cfg)

		if modified {
			t.Error("expected no modification when payload under limit")
		}
		if saved != 0 {
			t.Errorf("expected saved=0, got %d", saved)
		}
	})

	t.Run("payload over limit truncates", func(t *testing.T) {
		modified, saved := TrimPayload(logger, payload, 5, countTokens, cfg)

		if !modified {
			t.Error("expected modification when payload over limit")
		}
		if saved <= 0 {
			t.Errorf("expected saved>0, got %d", saved)
		}
	})
}