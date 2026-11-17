package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nenya/internal/infra"
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

	rel1, ok := g.EnterRequest()
	if !ok {
		t.Fatal("a live gateway must admit requests")
	}
	g.drain.mu.Lock()
	if g.drain.inflight != 1 {
		t.Fatalf("inflight = %d, want 1 after one admission", g.drain.inflight)
	}
	g.drain.mu.Unlock()

	rel2, ok := g.EnterRequest()
	if !ok {
		t.Fatal("a live gateway must admit a second request")
	}
	rel1()
	rel2()

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
	rel, ok := g.EnterRequest()
	if !ok {
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
	for {
		slip, ok := g.EnterRequest()
		if !ok {
			break
		}
		// An admission that slips in before the drain armed would break the
		// drain accounting — release it and wait for the arm to show up.
		slip()
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
	rel()
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
	rel, ok := g.EnterRequest()
	if !ok {
		t.Fatal("admission refused on a live gateway")
	}
	defer rel() // releases after the close already happened

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
	if rel, ok := g.EnterRequest(); ok {
		defer rel() // don't leak the admission slot on the failure path
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

// TestGatewayDrainGateDoubleExitCannotPreemptClose pins NENYA-333: an
// unpaired (spurious or double) release must never steal a live request's
// slot — the drain may complete only when the LAST real admission
// releases. Before the fix this failed: the silent `inflight > 0` guard in
// ExitRequest let an extra exit decrement the live request's slot to zero
// and close `drained` while a request was still being served (destroying
// SecureMem and MCP clients under it). The release closure now makes the
// pairing structural: the second invocation is counted, logged at Error,
// exported as nenya_drain_unpaired_exits_total, and does not touch the
// counter.
func TestGatewayDrainGateDoubleExitCannotPreemptClose(t *testing.T) {
	var logBuf bytes.Buffer
	g := &NenyaGateway{
		Logger:  slog.New(slog.NewTextHandler(&logBuf, nil)),
		Metrics: infra.NewMetrics(),
	}

	rel1, ok := g.EnterRequest()
	if !ok {
		t.Fatal("admission refused on a live gateway")
	}
	rel2, ok := g.EnterRequest()
	if !ok {
		t.Fatal("second admission refused on a live gateway")
	}

	closed := make(chan struct{})
	go func() {
		g.CloseWhenDrained(context.Background())
		close(closed)
	}()

	// First request finishes legitimately.
	rel1()

	// THE BUG CLASS: a double release of the same admission (double-defer,
	// a duplicated handler path, a future refactor mistake). It must be a
	// counted, loudly-logged no-op — not a decrement of the live slot.
	rel1()

	// The second request is still live: the drain must NOT have completed.
	select {
	case <-closed:
		t.Fatal("drain closed while a live request was still in flight: an unpaired exit stole its slot")
	case <-time.After(150 * time.Millisecond):
	}

	// The real last exit releases the waiter. The channel close is also the
	// happens-before edge that makes the log-buffer read below race-free
	// against the drain goroutine's own writes to the shared sink.
	rel2()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("CloseWhenDrained did not return after the last real request exited")
	}

	g.drain.mu.Lock()
	if g.drain.unpairedExits != 1 {
		t.Errorf("unpairedExits = %d, want 1 after the double release", g.drain.unpairedExits)
	}
	g.drain.mu.Unlock()
	if !strings.Contains(logBuf.String(), "unpaired gateway exit") {
		t.Error("the double release was not logged as an unpaired gateway exit")
	}

	// The misuse counter must be wired: render the Prometheus output and
	// assert the drain counter survived field/export naming.
	var prom bytes.Buffer
	g.Metrics.WritePrometheus(&prom)
	if !strings.Contains(prom.String(), "nenya_drain_unpaired_exits_total 1") {
		t.Errorf("nenya_drain_unpaired_exits_total not exported with value 1; output:\n%s", prom.String())
	}
}
