package proxy

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
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

func TestBouncerInterceptorFailClosed(t *testing.T) {
	newReq := func() *pipeline.InterceptRequest {
		return &pipeline.InterceptRequest{
			Payload: map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "very large payload"}},
			},
			Messages: []map[string]any{{"role": "user", "content": "very large payload"}},
		}
	}

	t.Run("fail_open disabled rejects with bouncer error", func(t *testing.T) {
		gw := &gateway.NenyaGateway{
			Logger: slog.New(slog.DiscardHandler),
			Config: config.Config{
				Bouncer: config.BouncerConfig{FailOpen: config.PtrTo(false)},
			},
		}
		interceptor := NewBouncerInterceptor(gw, gw.Logger)

		_, err := interceptor.Process(context.Background(), newReq())
		var reject *pipeline.RejectError
		if !errors.As(err, &reject) {
			t.Fatalf("Process() error = %v, want *RejectError", err)
		}
		if reject.Kind != infra.ErrorKindBouncerError {
			t.Errorf("Kind = %q, want %q", reject.Kind, infra.ErrorKindBouncerError)
		}
		if !strings.Contains(err.Error(), "fail_open is disabled") {
			t.Errorf("error text missing fail-closed reason: %v", err)
		}
	})

	t.Run("fail_open default skips on engine failure", func(t *testing.T) {
		gw := &gateway.NenyaGateway{
			Logger: slog.New(slog.DiscardHandler),
			Config: config.Config{
				Bouncer: config.BouncerConfig{FailOpen: config.PtrTo(true)},
			},
		}
		interceptor := NewBouncerInterceptor(gw, gw.Logger)

		result, err := interceptor.Process(context.Background(), newReq())
		if err != nil {
			t.Fatalf("Process() error = %v, want skip", err)
		}
		if !result.Skip {
			t.Error("expected Skip=true under fail-open")
		}
	})

	t.Run("EffectiveFailOpen nil defaults true", func(t *testing.T) {
		cfg := config.BouncerConfig{}
		if !cfg.EffectiveFailOpen() {
			t.Error("nil FailOpen must resolve to fail-open")
		}
	})
}

func TestBouncerInterceptorFailClosedSkipPaths(t *testing.T) {
	failClosedGW := func() *gateway.NenyaGateway {
		return &gateway.NenyaGateway{
			Logger: slog.New(slog.DiscardHandler),
			Config: config.Config{
				Bouncer: config.BouncerConfig{FailOpen: config.PtrTo(false)},
			},
		}
	}

	t.Run("null content skips even fail-closed", func(t *testing.T) {
		interceptor := NewBouncerInterceptor(failClosedGW(), slog.New(slog.DiscardHandler))
		req := &pipeline.InterceptRequest{
			Payload:  map[string]any{"messages": []any{}},
			Messages: []map[string]any{{"role": "assistant", "content": nil, "tool_calls": []any{}}},
		}
		res, err := interceptor.Process(context.Background(), req)
		if err != nil {
			t.Fatalf("Process() error = %v, want skip", err)
		}
		if !res.Skip {
			t.Error("null content must skip")
		}
	})

	t.Run("empty string content skips even fail-closed", func(t *testing.T) {
		interceptor := NewBouncerInterceptor(failClosedGW(), slog.New(slog.DiscardHandler))
		req := &pipeline.InterceptRequest{
			Payload:  map[string]any{"messages": []any{}},
			Messages: []map[string]any{{"role": "user", "content": ""}},
		}
		res, err := interceptor.Process(context.Background(), req)
		if err != nil {
			t.Fatalf("Process() error = %v, want skip", err)
		}
		if !res.Skip {
			t.Error("empty string content must skip")
		}
	})

	t.Run("rich content rejects under fail-closed", func(t *testing.T) {
		interceptor := NewBouncerInterceptor(failClosedGW(), slog.New(slog.DiscardHandler))
		req := &pipeline.InterceptRequest{
			Payload: map[string]any{"messages": []any{}},
			Messages: []map[string]any{{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "very large rich payload"},
			}}},
		}
		_, err := interceptor.Process(context.Background(), req)
		var reject *pipeline.RejectError
		if !errors.As(err, &reject) {
			t.Fatalf("Process() error = %v, want *RejectError for rich content", err)
		}
		if reject.Kind != infra.ErrorKindBouncerError {
			t.Errorf("Kind = %q, want %q", reject.Kind, infra.ErrorKindBouncerError)
		}
	})

	t.Run("rich content skips under fail-open", func(t *testing.T) {
		gw := &gateway.NenyaGateway{
			Logger: slog.New(slog.DiscardHandler),
			Config: config.Config{Bouncer: config.BouncerConfig{FailOpen: config.PtrTo(true)}},
		}
		interceptor := NewBouncerInterceptor(gw, gw.Logger)
		req := &pipeline.InterceptRequest{
			Payload:  map[string]any{"messages": []any{}},
			Messages: []map[string]any{{"role": "user", "content": []any{map[string]any{"type": "text", "text": "x"}}}},
		}
		res, err := interceptor.Process(context.Background(), req)
		if err != nil {
			t.Fatalf("Process() error = %v, want skip", err)
		}
		if !res.Skip {
			t.Error("rich content must skip under fail-open")
		}
	})

	t.Run("disabled bouncer skips", func(t *testing.T) {
		gw := &gateway.NenyaGateway{
			Logger: slog.New(slog.DiscardHandler),
			Config: config.Config{Bouncer: config.BouncerConfig{Enabled: config.PtrTo(false)}},
		}
		interceptor := NewBouncerInterceptor(gw, gw.Logger)
		req := &pipeline.InterceptRequest{
			Payload:  map[string]any{"messages": []any{}},
			Messages: []map[string]any{{"role": "user", "content": "x"}},
		}
		if interceptor.CanHandle(context.Background(), req) {
			t.Error("disabled bouncer must not handle")
		}
	})
}
