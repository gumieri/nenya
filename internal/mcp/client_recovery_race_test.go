package mcp

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestClient_RecoverTransportSwapRaceFree pins the AUD-09 fix (NENYA-217):
// the recovery transport swap must be synchronized against concurrent
// transport readers. Pre-fix, reinitialize wrote c.transport without the
// mutex while currentTransport (dispatch capture) and Close read it under
// RLock — a plain happens-before violation the race detector flagged,
// surfacing as the intermittent TestClient_Recover_ConcurrentSingleFlight
// failure under -race. Run with -race; this test asserts post-condition
// health on top of race-freedom so it is never vacuous.
func TestClient_RecoverTransportSwapRaceFree(t *testing.T) {
	mock := newMockMCPServer(t)

	client := NewClient(ClientConfig{
		Name:   "nenya-test",
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	stop := make(chan struct{})
	var wg sync.WaitGroup
	// Concurrent transport readers — the dispatch path's capture shape
	// (RLock'd field reads racing the recovery swap).
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				tr := client.currentTransport()
				if tr == nil {
					t.Error("the recovery swap installed a nil transport")
					return
				}
				runtime.Gosched()
			}
		}()
	}

	// Serial recoveries swap the transport while the readers run. Each
	// Recover on a healthy client rebuilds: the readiness fast-path only
	// short-circuits a REcovered generation.
	const rebuilds = 5
	for range rebuilds {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := client.Recover(ctx); err != nil {
			cancel()
			t.Fatalf("Recover: %v", err)
		}
		cancel()
	}
	close(stop)
	wg.Wait()

	// Post-condition: the client still works after the churn, and the
	// mock saw the initial session plus one connect per rebuild.
	if _, err := client.RefreshTools(context.Background()); err != nil {
		t.Fatalf("RefreshTools after recovery churn: %v", err)
	}
	if got := mock.ConnectCount(); got != 1+rebuilds {
		t.Fatalf("expected %d SSE sessions (initial + one per rebuild), got %d", 1+rebuilds, got)
	}
}

// TestClient_CloseDuringRecoveryDoesNotResurrect pins the AUD-09 review fix:
// a Close landing while a recovery handshake is in flight must win — the
// completing recovery refuses to re-mark the client initialized and tears
// the fresh transport down, leaving no live SSE session on a closed client
// (realistic under the reload drain, where Close lands while in-flight
// CallTools still recover).
func TestClient_CloseDuringRecoveryDoesNotResurrect(t *testing.T) {
	mock := newMockMCPServer(t)

	client := NewClient(ClientConfig{
		Name:   "nenya-test",
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// Recovery churn in the background; Close lands mid-churn.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for gen := 0; gen < 20; gen++ {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = client.Recover(ctx)
			cancel()
		}
	}()
	time.Sleep(20 * time.Millisecond)
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()

	// The closed gate must hold: no resurrected initialized state.
	if client.initialized.Load() {
		t.Fatal("a client closed during recovery churn was resurrected (initialized=true)")
	}
}
