package pipeline

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"testing"

	"github.com/nenya/internal/infra"
)

// NENYA-353 pin: the redaction interceptor must raise SecretsRedacted in
// place on the InterceptRequest when its secret patterns match — the
// tool-egress policy's sensitivity signal depends on it. Pre-fix the flag
// stays false even though redactions were applied.
func TestRedactInterceptorSetsSecretsRedacted(t *testing.T) {
	patterns := []*regexp.Regexp{regexp.MustCompile(`sk-[a-zA-Z0-9]{20,}`)}
	interceptor := NewRedactInterceptor(true, patterns, "REDACTED", infra.NewMetrics())

	req := &InterceptRequest{
		Payload:  map[string]any{"model": "demo"},
		Messages: []map[string]any{{"role": "user", "content": "rotate sk-ABCDEFGHIJKLMNOPQRSTUVWXYZ1234 now"}},
	}
	if _, err := interceptor.Process(context.Background(), req); err != nil {
		t.Fatalf("process: %v", err)
	}
	if !req.SecretsRedacted {
		t.Fatal("secret-pattern redaction did not raise SecretsRedacted")
	}

	clean := &InterceptRequest{
		Payload:  map[string]any{"model": "demo"},
		Messages: []map[string]any{{"role": "user", "content": "nothing sensitive here"}},
	}
	if _, err := interceptor.Process(context.Background(), clean); err != nil {
		t.Fatalf("process clean: %v", err)
	}
	if clean.SecretsRedacted {
		t.Fatal("clean request must not raise SecretsRedacted")
	}
}

// TestRedactInterceptorSecretsRedactedNotSetWhenDisabled guards the flag
// against a disabled interceptor: through the chain (which consults
// CanHandle), a disabled redactor redacts nothing and raises no signal.
func TestRedactInterceptorSecretsRedactedNotSetWhenDisabled(t *testing.T) {
	patterns := []*regexp.Regexp{regexp.MustCompile(`sk-[a-zA-Z0-9]{20,}`)}
	chain := NewInterceptorChain(slog.New(slog.NewTextHandler(io.Discard, nil)))
	chain.Register(NewRedactInterceptor(false, patterns, "REDACTED", infra.NewMetrics()))

	req := &InterceptRequest{
		Payload:  map[string]any{"model": "demo"},
		Messages: []map[string]any{{"role": "user", "content": "rotate sk-ABCDEFGHIJKLMNOPQRSTUVWXYZ1234 now"}},
	}
	if _, err := chain.Execute(context.Background(), req); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if req.SecretsRedacted {
		t.Fatal("disabled interceptor must not raise SecretsRedacted")
	}
}
