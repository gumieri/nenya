package pipeline

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/nenya/internal/tracing"
)

// TestExecuteEmitsTraceSpan verifies the NENYA-140 stage-span emission:
// chain Execute writes one "trace span" record naming the stage, and the
// stage is recorded into the request summary when a trace is active.
func TestExecuteEmitsTraceSpan(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	chain := NewInterceptorChain(logger)
	chain.Register(&stubInterceptor{name: "tfidf", priority: 30})
	req := &InterceptRequest{Payload: map[string]any{"model": "m"}, Messages: []map[string]any{}}

	ctx, rt := tracing.StartRequest(context.Background(), logger, "", "chat_completions")
	if _, err := chain.Execute(ctx, req); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	rt.Finish()

	out := buf.String()
	if !strings.Contains(out, "trace span") || !strings.Contains(out, "span=pipeline.interceptors") {
		t.Fatalf("expected a trace span record for the stage, got:\n%s", out)
	}
	if !strings.Contains(out, "request trace") || !strings.Contains(out, "stages=[pipeline.interceptors]") {
		t.Fatalf("expected the request summary with the recorded stage, got:\n%s", out)
	}
}

// TestExecuteUntracedIsInert verifies a context without a request trace
// still executes (root span) without panicking — the spans simply root a
// fresh trace.
func TestExecuteUntracedIsInert(t *testing.T) {
	chain := NewInterceptorChain(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	chain.Register(&stubInterceptor{name: "tfidf", priority: 30})
	if _, err := chain.Execute(context.Background(), &InterceptRequest{Payload: map[string]any{}}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}
