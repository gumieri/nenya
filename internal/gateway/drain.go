package gateway

import (
	"context"
	"sync"
)

// drainGate serializes request admission against gateway disposal
// (AUD-07 / NENYA-215). A reload swaps the serving gateway while in-flight
// requests still reference the old instance: SecureMem (provider keys), MCP
// clients, the response cache and the quota fetcher must not be torn down
// under them. Proxy.ServeHTTP admits every request through the gateway's
// EnterRequest and releases it through ExitRequest; the disposal path
// (CloseWhenDrained) marks the gate draining — new requests are refused with
// 503 — and waits (bounded by its context) for the in-flight counter to
// reach zero before Close runs.
//
// Locking: mu guards draining, inflight and drained. drained is created
// under mu when draining is armed with work in flight, and is closed
// exactly once by the LAST ExitRequest (which nils it under the same lock).
// CloseWhenDrained waits on a snapshot taken under mu; a timeout abandons
// the wait and force-closes — a later ExitRequest closing the abandoned
// channel is harmless, and a second CloseWhenDrained re-reads the current
// channel under mu.
type drainGate struct {
	mu       sync.Mutex
	draining bool
	inflight int
	drained  chan struct{}
	// closeOnce guards the terminal Close: concurrent or repeated
	// CloseWhenDrained calls (reload drain racing process shutdown) close
	// the resources exactly once.
	closeOnce sync.Once
}

// EnterRequest admits one HTTP request to the gateway. It returns false once
// the gateway is draining: the request must be refused (503) instead of
// being served from resources that are about to be or already are closed.
func (g *NenyaGateway) EnterRequest() bool {
	g.drain.mu.Lock()
	defer g.drain.mu.Unlock()
	if g.drain.draining {
		return false
	}
	g.drain.inflight++
	return true
}

// ExitRequest releases one admitted request. The last exit while draining
// closes the drained channel, releasing the CloseWhenDrained waiter.
func (g *NenyaGateway) ExitRequest() {
	g.drain.mu.Lock()
	defer g.drain.mu.Unlock()
	if g.drain.inflight > 0 {
		g.drain.inflight--
	}
	if g.drain.inflight == 0 && g.drain.draining && g.drain.drained != nil {
		close(g.drain.drained)
		g.drain.drained = nil
	}
}

// Draining reports whether the gateway has stopped admitting requests
// (CloseWhenDrained armed) — observability for tests and drain logging.
func (g *NenyaGateway) Draining() bool {
	g.drain.mu.Lock()
	defer g.drain.mu.Unlock()
	return g.drain.draining
}

// CloseWhenDrained marks the gateway draining — refusing new admissions —
// waits for its in-flight requests to finish or for ctx to expire, whichever
// comes first, and then closes the gateway's resources exactly once. A
// context expiry is the documented last resort: the resources are closed
// under whatever requests remain (their behavior is undefined, as before
// the drain existed), and the expiry is logged at Warn with the residual
// in-flight count. The call is idempotent.
func (g *NenyaGateway) CloseWhenDrained(ctx context.Context) {
	g.drain.mu.Lock()
	if !g.drain.draining {
		g.drain.draining = true
		if g.drain.inflight > 0 {
			g.drain.drained = make(chan struct{})
		}
		g.Logger.Info("gateway draining for disposal", "inflight", g.drain.inflight)
	}
	ch := g.drain.drained
	g.drain.mu.Unlock()

	if ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
			// Report the RESIDUAL count, not the arm-time snapshot.
			g.drain.mu.Lock()
			remaining := g.drain.inflight
			g.drain.mu.Unlock()
			g.Logger.Warn("gateway drain timed out; closing under in-flight requests",
				"inflight", remaining,
				"err", ctx.Err())
		}
	}

	g.drain.closeOnce.Do(func() {
		g.Close()
	})
}
