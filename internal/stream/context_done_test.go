package stream

import (
	"context"
	"io"
	"testing"
)

// closeProbe records OnStreamClose notifications.
type closeProbe struct {
	events int
	closed int
	err    error
}

func (o *closeProbe) OnSSEEvent(event SSEEvent) { o.events++ }
func (o *closeProbe) OnStreamClose(err error) {
	o.closed++
	o.err = err
}

// TestFinishOnContextDoneNotifiesAndKeepsTerminator pins AUD-30
// (NENYA-249): when the request context is done mid-stream,
// finishOnContextDone must (a) still deliver the pending event terminator
// even when the first read after cancellation has a zero-length buffer, and
// (b) notify OnStreamClose exactly once with the context error. Pre-fix the
// zero-length read dropped the terminator AND the close notification was
// skipped on this path entirely.
func TestFinishOnContextDoneNotifiesAndKeepsTerminator(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := &closeProbe{}
	pr, pw := io.Pipe()
	// The wire carries the data line but NOT its terminating blank line:
	// after the event is delivered, the event separator is still pending
	// (emittedAny && !lastLineBlank) when the context flips.
	go func() {
		_, _ = pw.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n"))
	}()
	reader := NewSSETransformingReader(pr, nil, ctx)
	reader.SetObserver(probe)

	// First read delivers the event, leaving the separator pending.
	buf := make([]byte, 512)
	n, err := reader.Read(buf)
	if n == 0 || err != nil {
		t.Fatalf("event read = (%d, %v), want the event bytes", n, err)
	}

	// Cancel AFTER the event: the separator is now pending.
	cancel()

	// First read after cancellation with a ZERO-LENGTH buffer: the pending
	// terminator must survive it.
	if zn, zerr := reader.Read([]byte{}); zn != 0 || zerr != nil {
		t.Fatalf("zero-length read = (%d, %v), want (0, nil) deferring termination", zn, zerr)
	}

	// Next read delivers the terminator.
	n, err = reader.Read(buf)
	if n == 0 {
		t.Fatalf("the pending terminator was dropped: read = (%d, %v)", n, err)
	}

	// The stream then terminates with the context error, and the observer
	// saw exactly one close carrying it.
	if _, err := reader.Read(buf); err == nil {
		t.Fatal("expected the context error to terminate the stream")
	}
	if probe.closed != 1 {
		t.Fatalf("OnStreamClose called %d times, want exactly 1", probe.closed)
	}
	if probe.err == nil || probe.err != context.Canceled {
		t.Fatalf("OnStreamClose err = %v, want context.Canceled", probe.err)
	}
}
