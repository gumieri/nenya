package proxy

import (
	"context"
	"io"
	"testing"
	"time"
)

// TestDrainPendingStopsStallTimer pins AUD-48 (NENYA-231): DrainPending must
// retire the stall timer, not just close the stall channel. Pre-fix only
// Stop() stopped the timer, and the continuation path (finishStreamAttempt)
// calls DrainPending alone — leaving the timer armed for up to a FULL stall
// timeout after the response finished: the armed callback pins the reader,
// and a late read-loop delivery re-arms it (stalled was never set, so
// serveResult happily Reset the timer).
func TestDrainPendingStopsStallTimer(t *testing.T) {
	pr, pw := io.Pipe() // never written: the read loop blocks, the drain times out
	defer func() { _ = pw.Close() }()

	sr := newStallReader(context.Background(), pr, time.Hour, 0)

	if n, err := sr.DrainPending(20 * time.Millisecond); err == nil {
		t.Fatalf("expected the drain to time out, got n=%d, err=nil", n)
	}

	// The stall timer (1h) must no longer be pending: a still-active timer
	// reports true here. Pre-fix this fails — DrainPending left it armed.
	if sr.timer.Stop() {
		t.Fatal("the stall timer is still pending after DrainPending: it will fire up to a full stall timeout after the response is done, pinning the reader")
	}

	// A late delivery must not re-arm it either: post-fix the reader is
	// marked stalled, so serveResult bails out before its Reset.
	buf := make([]byte, 8)
	if n, err := sr.serveResult(buf, readResult{data: []byte("late")}); err != errStreamStalled {
		t.Fatalf("a late delivery after drain must be rejected as stalled, got n=%d err=%v", n, err)
	}
	if sr.timer.Stop() {
		t.Fatal("serveResult re-armed the stall timer after DrainPending")
	}
}
