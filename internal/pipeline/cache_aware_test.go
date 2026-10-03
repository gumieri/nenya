package pipeline

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/nenya/config"
)

// TestTFIDFCanHandle_SkipTFIDFPrune pins that the cache_aware flag makes
// TF-IDF decline the request without affecting other gating.
func TestTFIDFCanHandle_SkipTFIDFPrune(t *testing.T) {
	ic := NewTFIDFInterceptor(TFIDFInterceptorOpts{QuerySource: "self", Logger: slog.New(slog.DiscardHandler)})
	req := &InterceptRequest{
		Payload:    map[string]any{"model": "agent"},
		Messages:   []map[string]any{{"role": "user", "content": "prior"}, {"role": "user", "content": strings.Repeat("x", 5000)}},
		SoftLimit:  10,
		TokenCount: 100,
	}
	if !ic.CanHandle(context.Background(), req) {
		t.Fatal("expected CanHandle true without SkipTFIDFPrune")
	}
	req.SkipTFIDFPrune = true
	if ic.CanHandle(context.Background(), req) {
		t.Fatal("expected CanHandle false with SkipTFIDFPrune")
	}
}

// TestCacheAware_SecurityRunsAndTFIDFSkipped verifies that under cache_aware the
// history-mutating TF-IDF stage is skipped while a non-history (security-class)
// stage still runs. The generic stub stands in for a security interceptor; the
// real security interceptors never consult the flag.
func TestCacheAware_SecurityRunsAndTFIDFSkipped(t *testing.T) {
	security := &stubInterceptor{name: "redact", priority: 10}
	tfidf := NewTFIDFInterceptor(TFIDFInterceptorOpts{
		QuerySource: "self",
		ContextCfg:  config.ContextConfig{TruncationKeepFirstPct: 10, TruncationKeepLastPct: 10},
		Logger:      slog.New(slog.DiscardHandler),
	})
	chain := newTestChain(t, security, tfidf)

	content := strings.Repeat("word ", 2000)
	req := &InterceptRequest{
		Payload: map[string]any{"model": "agent", "messages": []any{
			map[string]any{"role": "user", "content": "prior"},
			map[string]any{"role": "user", "content": content},
		}},
		Messages: []map[string]any{
			{"role": "user", "content": "prior"},
			{"role": "user", "content": content},
		},
		SoftLimit:      10,
		HardLimit:      50,
		TokenCount:     5000,
		SkipTFIDFPrune: true,
	}

	if _, err := chain.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !security.handled {
		t.Error("security interceptor did not run under cache_aware")
	}
	if req.Messages[1]["content"] != content {
		t.Error("TF-IDF mutated the payload despite SkipTFIDFPrune")
	}
}
