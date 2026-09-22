package proxy

import (
	"context"
	"log/slog"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/pipeline"
)

func TestBouncerInterceptorCanHandle(t *testing.T) {
	tests := []struct {
		name          string
		tokenCount    int
		softLimit     int
		ctxCanceled   bool
		enabled       *bool
		wantCanHandle bool
	}{
		{
			name:          "handles when tokens exceed soft_limit",
			tokenCount:    5000,
			softLimit:     4000,
			ctxCanceled:   false,
			enabled:       config.PtrTo(true),
			wantCanHandle: true,
		},
		{
			name:          "does not handle when tokens below soft_limit",
			tokenCount:    3000,
			softLimit:     4000,
			ctxCanceled:   false,
			enabled:       config.PtrTo(true),
			wantCanHandle: false,
		},
		{
			name:          "does not handle when soft_limit is zero (unknown MaxContext)",
			tokenCount:    5000,
			softLimit:     0,
			ctxCanceled:   false,
			enabled:       config.PtrTo(true),
			wantCanHandle: false,
		},
		{
			name:          "does not handle when context canceled",
			tokenCount:    5000,
			softLimit:     4000,
			ctxCanceled:   true,
			enabled:       config.PtrTo(true),
			wantCanHandle: false,
		},
		{
			name:          "handles when tokens equal soft_limit",
			tokenCount:    4000,
			softLimit:     4000,
			ctxCanceled:   false,
			enabled:       config.PtrTo(true),
			wantCanHandle: true,
		},
		{
			name:          "does not handle when bouncer explicitly disabled",
			tokenCount:    5000,
			softLimit:     4000,
			ctxCanceled:   false,
			enabled:       config.PtrTo(false),
			wantCanHandle: false,
		},
		{
			name:          "handles when enabled is unset (defaults to enabled)",
			tokenCount:    5000,
			softLimit:     4000,
			ctxCanceled:   false,
			wantCanHandle: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := &gateway.NenyaGateway{
				Logger: slog.Default(),
				Config: config.Config{
					Bouncer: config.BouncerConfig{Enabled: tt.enabled},
				},
			}
			interceptor := NewBouncerInterceptor(gw, slog.Default())

			var ctx context.Context
			if tt.ctxCanceled {
				canceledCtx, cancel := context.WithCancel(context.Background())
				cancel()
				ctx = canceledCtx
			} else {
				ctx = context.Background()
			}

			req := &pipeline.InterceptRequest{
				TokenCount: tt.tokenCount,
				SoftLimit:  tt.softLimit,
			}
			got := interceptor.CanHandle(ctx, req)
			if got != tt.wantCanHandle {
				t.Errorf("CanHandle() = %v, want %v", got, tt.wantCanHandle)
			}
		})
	}
}

func TestResolvePostTrimMessage(t *testing.T) {
	t.Run("string content resolves", func(t *testing.T) {
		payload := map[string]any{
			"messages": []interface{}{
				map[string]any{"role": "user", "content": "trimmed text"},
			},
		}
		m, text, ok := resolvePostTrimMessage(payload)
		if !ok || text != "trimmed text" || m == nil {
			t.Fatalf("expected resolution, got ok=%v text=%q", ok, text)
		}
	})

	t.Run("null content skips", func(t *testing.T) {
		payload := map[string]any{
			"messages": []interface{}{
				map[string]any{"role": "assistant", "content": nil},
			},
		}
		if _, _, ok := resolvePostTrimMessage(payload); ok {
			t.Error("expected ok=false for null content")
		}
	})

	t.Run("system role skips", func(t *testing.T) {
		payload := map[string]any{
			"messages": []interface{}{
				map[string]any{"role": "system", "content": "the system prompt"},
			},
		}
		if _, _, ok := resolvePostTrimMessage(payload); ok {
			t.Error("expected ok=false for system-role last message")
		}
	})

	t.Run("non-map last message skips", func(t *testing.T) {
		payload := map[string]any{
			"messages": []interface{}{"not a map"},
		}
		if _, _, ok := resolvePostTrimMessage(payload); ok {
			t.Error("expected ok=false for non-map message")
		}
	})
}
