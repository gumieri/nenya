package gateway

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// newDrainTestGateway builds a bare gateway for the drain-gate tests: the
// drain gate is a zero-value struct and Close is nil-safe for every
// resource type, so no config assembly is needed.
func newDrainTestGateway() *NenyaGateway {
	return &NenyaGateway{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestGatewayDrainGateAdmission pins the admission counter: every admitted
// request increments it, every exit decrements it, and admission stays open
// while the gateway is live.
func TestGatewayDrainGateAdmission(t *testing.T) {
	g := newDrainTestGateway()

	if !g.EnterRequest() {
		t.Fatal("a live gateway must admit requests")
	}
	g.drain.mu.Lock()
	if g.drain.inflight != 1 {
		t.Fatalf("inflight = %d, want 1 after one admission", g.drain.inflight)
	}
	g.drain.mu.Unlock()

	if !g.EnterRequest() {
		t.Fatal("a live gateway must admit a second request")
	}
	g.ExitRequest()
	g.ExitRequest()

	g.drain.mu.Lock()
	defer g.drain.mu.Unlock()
	if g.drain.inflight != 0 {
		t.Fatalf("inflight = %d, want 0 after balanced exits", g.drain.inflight)
	}
	if g.drain.draining {
		t.Fatal("a live gateway must not be draining")
	}
}

// TestGatewayCloseWhenDrainedWaits pins the drain: CloseWhenDrained must
// refuse new admissions immediately but wait for the held in-flight request
// to exit before it closes the gateway.
func TestGatewayCloseWhenDrainedWaits(t *testing.T) {
	g := newDrainTestGateway()
	if !g.EnterRequest() {
		t.Fatal("admission refused on a live gateway")
	}

	closed := make(chan struct{})
	go func() {
		g.CloseWhenDrained(context.Background())
		close(closed)
	}()

	// Draining arms synchronously enough for this poll: admission is now
	// refused even though the close has not happened.
	deadline := time.Now().Add(2 * time.Second)
	for g.EnterRequest() {
		// An admission that slips in before the drain armed would break the
		// drain accounting — release it and wait for the arm to show up.
		g.ExitRequest()
		if time.Now().After(deadline) {
			t.Fatal("CloseWhenDrained never armed the drain gate")
		}
		time.Sleep(time.Millisecond)
	}
	if !g.Draining() {
		t.Fatal("Draining() = false while CloseWhenDrained is in flight")
	}

	// The held request keeps the gateway open.
	select {
	case <-closed:
		t.Fatal("CloseWhenDrained closed the gateway under an in-flight request")
	case <-time.After(150 * time.Millisecond):
	}

	// Exit releases the waiter and the close completes.
	g.ExitRequest()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("CloseWhenDrained did not return after the last in-flight request exited")
	}
}

// TestGatewayCloseWhenDrainedTimeoutForces pins the last-resort path: when
// the in-flight request outlives the context, the close proceeds anyway
// (documented behavior — resources close under whatever remains).
func TestGatewayCloseWhenDrainedTimeoutForces(t *testing.T) {
	g := newDrainTestGateway()
	if !g.EnterRequest() {
		t.Fatal("admission refused on a live gateway")
	}
	defer g.ExitRequest() // releases after the close already happened

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		g.CloseWhenDrained(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("CloseWhenDrained did not force-close at the context deadline")
	}
}

// TestGatewayCloseWhenDrainedIdempotent pins the exact-once close: repeated
// CloseWhenDrained calls on an idle gateway are safe no-ops past the first.
func TestGatewayCloseWhenDrainedIdempotent(t *testing.T) {
	g := newDrainTestGateway()
	g.CloseWhenDrained(context.Background())
	if !g.Draining() {
		t.Fatal("gateway not draining after CloseWhenDrained")
	}
	g.CloseWhenDrained(context.Background())
	if g.EnterRequest() {
		t.Fatal("a drained gateway must refuse admission")
	}
}

// TestGatewayCloseWhenDrainedIdleImmediate pins the no-work fast path: with
// nothing in flight the drain completes without waiting a tick.
func TestGatewayCloseWhenDrainedIdleImmediate(t *testing.T) {
	g := newDrainTestGateway()
	done := make(chan struct{})
	go func() {
		g.CloseWhenDrained(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("CloseWhenDrained on an idle gateway did not complete immediately")
	}
}
