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
		ctxCancelled  bool
		enabled       *bool
		wantCanHandle bool
	}{
		{
			name:          "handles when tokens exceed soft_limit",
			tokenCount:    5000,
			softLimit:     4000,
			ctxCancelled:  false,
			enabled:       config.PtrTo(true),
			wantCanHandle: true,
		},
		{
			name:          "does not handle when tokens below soft_limit",
			tokenCount:    3000,
			softLimit:     4000,
			ctxCancelled:  false,
			enabled:       config.PtrTo(true),
			wantCanHandle: false,
		},
		{
			name:          "does not handle when soft_limit is zero (unknown MaxContext)",
			tokenCount:    5000,
			softLimit:     0,
			ctxCancelled:  false,
			enabled:       config.PtrTo(true),
			wantCanHandle: false,
		},
		{
			name:          "does not handle when context cancelled",
			tokenCount:    5000,
			softLimit:     4000,
			ctxCancelled:  true,
			enabled:       config.PtrTo(true),
			wantCanHandle: false,
		},
		{
			name:          "does not handle when tokens equal soft_limit",
			tokenCount:    4000,
			softLimit:     4000,
			ctxCancelled:  false,
			enabled:       config.PtrTo(true),
			wantCanHandle: true,
		},
		{
			name:          "does not handle when bouncer explicitly disabled",
			tokenCount:    5000,
			softLimit:     4000,
			ctxCancelled:  false,
			enabled:       config.PtrTo(false),
			wantCanHandle: false,
		},
		{
			name:          "handles when enabled is unset (defaults to enabled)",
			tokenCount:    5000,
			softLimit:     4000,
			ctxCancelled:  false,
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
			if tt.ctxCancelled {
				cancelledCtx, cancel := context.WithCancel(context.Background())
				cancel()
				ctx = cancelledCtx
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
