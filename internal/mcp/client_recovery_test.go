package mcp

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestClient_RecoverAfterExpiredSession pins NENYA-21: a tools/call POST
// rejected at the HTTP layer (expired/broken session) triggers a single
// transparent recovery — transport rebuild + handshake — and the call is
// replayed exactly once, returning the tool result.
func TestClient_RecoverAfterExpiredSession(t *testing.T) {
	mock := newMockMCPServer(t)

	client := NewClient(ClientConfig{
		Name:   "nenya-test",
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() }) // release the live SSE session
	if _, err := client.RefreshTools(context.Background()); err != nil {
		t.Fatalf("RefreshTools: %v", err)
	}

	mock.BreakToolCalls()

	result, err := client.CallTool(context.Background(), "test_tool", map[string]any{"query": "ping"})
	if err != nil {
		t.Fatalf("CallTool should recover and replay, got error: %v", err)
	}
	if len(result.Content) == 0 || result.Content[0].Text != "result for: ping" {
		t.Fatalf("unexpected replayed result: %+v", result)
	}

	if got := mock.ConnectCount(); got != 2 {
		t.Fatalf("expected exactly 2 SSE sessions (initial + recovery), got %d", got)
	}
}

// TestClient_Recover_ConcurrentSingleFlight verifies concurrency safety:
// several tool calls hitting the same broken session produce exactly one
// transport rebuild, and every caller still gets its result.
func TestClient_Recover_ConcurrentSingleFlight(t *testing.T) {
	mock := newMockMCPServer(t)

	client := NewClient(ClientConfig{
		Name:   "nenya-test",
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() }) // release the live SSE session
	if _, err := client.RefreshTools(context.Background()); err != nil {
		t.Fatalf("RefreshTools: %v", err)
	}

	const callers = 4
	mock.BreakToolCalls()

	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, errs[slot] = client.CallTool(ctx, "test_tool", map[string]any{"query": "q"})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d failed: %v", i, err)
		}
	}

	// Initial session + exactly one recovery, despite `callers` concurrent
	// failures.
	if got := mock.ConnectCount(); got != 2 {
		t.Fatalf("expected exactly 2 SSE sessions under concurrency, got %d", got)
	}
}
