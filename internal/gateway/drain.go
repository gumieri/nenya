package gateway

import (
	"context"
	"runtime/debug"
	"sync"
)

// drainGate serializes request admission against gateway disposal
// (AUD-07 / NENYA-215). A reload swaps the serving gateway while in-flight
// requests still reference the old instance: SecureMem (provider keys), MCP
// clients, the response cache and the quota fetcher must not be torn down
// under them. Proxy.ServeHTTP admits every request through the gateway's
// EnterRequest and releases it through the returned release closure; the
// disposal path (CloseWhenDrained) marks the gate draining — new requests
// are refused with 503 — and waits (bounded by its context) for the
// in-flight counter to reach zero before Close runs.
//
// Locking: mu guards draining, inflight, unpairedExits and drained. drained
// is created under mu when draining is armed with work in flight, and is
// closed exactly once by the LAST release (which nils it under the same
// lock). CloseWhenDrained waits on a snapshot taken under mu; a timeout
// abandons the wait and force-closes — a later release closing the
// abandoned channel is harmless, and a second CloseWhenDrained re-reads the
// current channel under mu.
//
// Pairing (NENYA-333): admission returns a per-request release closure, so
// exit-without-enter is structurally impossible and a double release cannot
// steal a live request's slot — the second call is counted (unpairedExits,
// also exported as nenya_drain_unpaired_exits_total) and logged at Error
// instead of silently decrementing the counter and closing the drain under
// live requests.
type drainGate struct {
	mu       sync.Mutex
	draining bool
	inflight int
	drained  chan struct{}
	// unpairedExits counts release invocations that do not correspond to a
	// live admission (a double release). Pairing is structural (one closure
	// per admission), so any nonzero value signals internal misuse; it backs
	// the Error log, the Prometheus counter, and is observable in tests.
	// Each admission's released flag lives in its closure and is also
	// mu-guarded on every write.
	unpairedExits int
	// closeOnce guards the terminal Close: concurrent or repeated
	// CloseWhenDrained calls (reload drain racing process shutdown) close
	// the resources exactly once.
	closeOnce sync.Once
}

// EnterRequest admits one HTTP request to the gateway. ok is false once the
// gateway is draining: the request must be refused (503) instead of being
// served from resources that are about to be or already are closed. On ok
// the caller receives a release closure that exits exactly this admission —
// defer it; never call it more than once. release is a harmless no-op when
// ok is false, so an unconditional `defer release()` is always safe. A
// second invocation of a live release is also a no-op — one that counts as
// an unpaired exit (logged at Error, exported as
// nenya_drain_unpaired_exits_total) rather than corrupting the in-flight
// count (NENYA-333). release is safe for concurrent use and after a
// force-close timeout (a late release on an abandoned drain is harmless, as
// before).
func (g *NenyaGateway) EnterRequest() (release func(), ok bool) {
	g.drain.mu.Lock()
	defer g.drain.mu.Unlock()
	if g.drain.draining {
		return func() {}, false
	}
	g.drain.inflight++
	released := false
	// NENYA-177 note: this closure (plus the captured released flag) is a
	// deliberate per-admission allocation — the price of structural exit
	// pairing. Negligible next to per-request JSON work; revisit only if
	// profiles flag it.
	return func() {
		g.drain.mu.Lock()
		if released {
			// Snapshot under the lock, log OUTSIDE it: a slow sink must not
			// stall admissions during a misuse storm (same invariant as the
			// arm-time log in CloseWhenDrained).
			g.drain.unpairedExits++
			unpaired, inflight := g.drain.unpairedExits, g.drain.inflight
			g.drain.mu.Unlock()
			if g.Logger != nil {
				g.Logger.Error("unpaired gateway exit: a release ran twice for one admission",
					"inflight", inflight,
					"unpaired_exits", unpaired,
					"stack", string(debug.Stack()))
			}
			g.Metrics.RecordDrainUnpairedExits()
			return
		}
		released = true
		g.drain.inflight--
		if g.drain.inflight == 0 && g.drain.draining && g.drain.drained != nil {
			close(g.drain.drained)
			g.drain.drained = nil
		}
		g.drain.mu.Unlock()
	}, true
}

// Draining reports whether the gateway has stopped admitting requests
// (CloseWhenDrained armed) — test observability.
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
		// Snapshot before logging: a slow log sink must not stall concurrent
		// EnterRequest/release calls for the write duration (the timeout Warn
		// below already logs outside the lock — mirror it).
		armingInflight := g.drain.inflight
		g.drain.mu.Unlock()
		if g.Logger != nil {
			g.Logger.Info("gateway draining for disposal", "inflight", armingInflight)
		}
		g.drain.mu.Lock()
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
			if g.Logger != nil {
				g.Logger.Warn("gateway drain timed out; closing under in-flight requests",
					"inflight", remaining,
					"err", ctx.Err())
			}
		}
	}

	g.drain.closeOnce.Do(func() {
		g.Close()
	})
}
