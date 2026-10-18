package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// NENYA-177: the interceptor chain mutates message maps in place — a
// read-only pass must not clone the payload. The fixture is deliberately large
// (40 messages): a deep clone of it costs hundreds of allocations, so the
// ceiling actually discriminates, while a clone-free pass stays flat in the
// number of messages. The second test proves the in-place contract directly.
func TestInterceptorChainDoesNotClonePayload(t *testing.T) {
	const messageCount = 40
	build := func() (map[string]any, []map[string]any) {
		msgs := make([]map[string]any, 0, messageCount)
		raw := make([]any, 0, messageCount)
		for range messageCount {
			msg := map[string]any{"role": "user", "content": strings.Repeat("x", 1024)}
			msgs = append(msgs, msg)
			raw = append(raw, msg)
		}
		return map[string]any{"model": "m", "messages": raw}, msgs
	}

	passAllocs := testing.AllocsPerRun(20, func() {
		payload, msgs := build()
		req := &InterceptRequest{Payload: payload, Messages: msgs}
		chain := &InterceptorChain{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		// One registered interceptor widens the pin beyond Execute's own
		// overhead: a clone introduced inside a real Process implementation
		// also trips the ceiling.
		chain.Register(&mutatingTestInterceptor{})
		if _, err := chain.Execute(t.Context(), req); err != nil {
			t.Fatalf("chain pass: %v", err)
		}
	})
	// The baseline is a deep clone of the same fixture (the regression this
	// guards against): the pass must stay clearly below it, so the ceiling
	// discriminates instead of being a magic number.
	cloneAllocs := testing.AllocsPerRun(20, func() {
		payload, _ := build()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		var clone map[string]any
		if err := json.Unmarshal(raw, &clone); err != nil {
			t.Fatalf("unmarshal fixture: %v", err)
		}
	})
	if passAllocs >= cloneAllocs {
		t.Fatalf("read-only pass allocated %.0f objects for %d messages, at or above the %.0f-object deep-clone baseline: a payload clone may have been reintroduced",
			passAllocs, messageCount, cloneAllocs)
	}
}

// TestInterceptorChainMutatesInPlace pins the no-clone contract: a mutating
// interceptor's rewrite must be visible on the caller's message map, which is
// only true when the chain walks the payload in place.
func TestInterceptorChainMutatesInPlace(t *testing.T) {
	msg := map[string]any{"role": "user", "content": "secret"}
	payload := map[string]any{"model": "m", "messages": []any{msg}}
	req := &InterceptRequest{Payload: payload, Messages: []map[string]any{msg}}

	chain := &InterceptorChain{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	chain.Register(&mutatingTestInterceptor{})
	if _, err := chain.Execute(t.Context(), req); err != nil {
		t.Fatalf("chain pass: %v", err)
	}
	if got := msg["content"]; got != "rewritten" {
		t.Fatalf("in-place rewrite not visible on the caller's map: %v", got)
	}
}

// mutatingTestInterceptor rewrites every text surface.
type mutatingTestInterceptor struct{}

func (m *mutatingTestInterceptor) Name() string  { return "mutating-test" }
func (m *mutatingTestInterceptor) Priority() int { return 1 }
func (m *mutatingTestInterceptor) CanHandle(context.Context, *InterceptRequest) bool {
	return true
}
func (m *mutatingTestInterceptor) Process(_ context.Context, req *InterceptRequest) (*InterceptResult, error) {
	for _, msg := range req.Messages {
		WalkMessageText(msg, func(string) string { return "rewritten" })
	}
	return &InterceptResult{Payload: req.Payload}, nil
}
