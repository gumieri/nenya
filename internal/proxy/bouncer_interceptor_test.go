package proxy

import (
	"context"
	"log/slog"
	"testing"

	"git.0ur.uk/nenya/internal/gateway"
	"git.0ur.uk/nenya/internal/pipeline"
)

func TestBouncerInterceptorCanHandle(t *testing.T) {
	tests := []struct {
		name         string
		tokenCount   int
		softLimit    int
		ctxCancelled bool
		wantCanHandle bool
	}{
		{
			name:         "handles when tokens exceed soft_limit",
			tokenCount:   5000,
			softLimit:    4000,
			ctxCancelled: false,
			wantCanHandle: true,
		},
		{
			name:         "does not handle when tokens below soft_limit",
			tokenCount:   3000,
			softLimit:    4000,
			ctxCancelled: false,
			wantCanHandle: false,
		},
		{
			name:         "does not handle when soft_limit is zero (unknown MaxContext)",
			tokenCount:   5000,
			softLimit:    0,
			ctxCancelled: false,
			wantCanHandle: false,
		},
		{
			name:         "does not handle when context cancelled",
			tokenCount:   5000,
			softLimit:    4000,
			ctxCancelled: true,
			wantCanHandle: false,
		},
		{
			name:         "does not handle when tokens equal soft_limit",
			tokenCount:   4000,
			softLimit:    4000,
			ctxCancelled: false,
			wantCanHandle: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := &gateway.NenyaGateway{
				Logger: slog.Default(),
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