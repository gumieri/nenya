package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/testutil"
)

// newStallTimeoutProxy builds a one-target streaming proxy with the per-write
// client-stall deadline (NENYA-155) set to the given seconds.
func newStallTimeoutProxy(t *testing.T, upstreamURL string, seconds int) *Proxy {
	t.Helper()
	p := newBootstrapProxy(t, []string{upstreamURL}, 0)
	gw := p.Gateway()
	gw.Config.Server.StreamStallTimeoutSeconds = config.PtrTo(seconds)
	// Keep the head probes out of the way: the scenario is a mid-stream stall.
	gw.Config.Governance.StreamBootstrapBufferBytes = 0
	gw.Config.Governance.EmptyStreamAsError = config.PtrTo(false)
	gw.Config.Governance.EarlyStreamErrorFailover = config.PtrTo(false)
	gw.Config.Governance.StreamIdleTimeoutSeconds = config.PtrTo(30)
	return p
}

// stuckClientWriter models a peer that reads the handshake and then stops
// draining: writes after freeWrites block, and a SetWriteDeadline arms a real
// timeout that unblocks the pending write with a net.Error timeout — exactly
// what the kernel does to a full socket.
type stuckClientWriter struct {
	header     http.Header
	code       int
	mu         sync.Mutex
	count      int
	freeWrites int
	deadline   chan struct{}
	fireOnce   sync.Once
}

func newStuckClientWriter(freeWrites int) *stuckClientWriter {
	return &stuckClientWriter{
		header:     make(http.Header),
		freeWrites: freeWrites,
		deadline:   make(chan struct{}),
	}
}

func (b *stuckClientWriter) Header() http.Header { return b.header }

func (b *stuckClientWriter) WriteHeader(code int) { b.code = code }

func (b *stuckClientWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.count++
	n := b.count
	b.mu.Unlock()
	if n > b.freeWrites {
		<-b.deadline
		return 0, &fakeTimeoutError{}
	}
	return len(p), nil
}

func (b *stuckClientWriter) Flush() {}

// SetWriteDeadline arms the timeout the ResponseController asks for.
func (b *stuckClientWriter) SetWriteDeadline(t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		b.fire()
		return nil
	}
	time.AfterFunc(d, b.fire)
	return nil
}

// fire closes the deadline exactly once (re-armed timers can overlap).
func (b *stuckClientWriter) fire() {
	b.fireOnce.Do(func() { close(b.deadline) })
}

// fakeTimeoutError is the net.Error a full socket surfaces on deadline.
type fakeTimeoutError struct{}

func (e *fakeTimeoutError) Error() string { return "write deadline exceeded (fake stuck peer)" }
func (e *fakeTimeoutError) Timeout() bool { return true }
func (e *fakeTimeoutError) Temporary() bool {
	return true
}

// TestStuckClientEvictedByWriteDeadline pins NENYA-155 end to end: a client
// that stops reading mid-stream must be evicted by the per-write deadline —
// the upstream is canceled, the stall is classified client_stalled with only
// telemetry (no provider cooldown or breaker charge), and the handler
// goroutine returns instead of pinning the upstream connection.
func TestStuckClientEvictedByWriteDeadline(t *testing.T) {
	upstreamDone := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"go\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		chunk := strings.Repeat("x", 64*1024)
		for i := 0; i < 1000; i++ {
			if _, err := w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"" + chunk + "\"}}]}\n\n")); err != nil {
				upstreamDone <- struct{}{}
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer up.Close()

	p := newStallTimeoutProxy(t, up.URL, 1) // 1s per-write deadline

	client := newStuckClientWriter(2) // role chunk + first content chunk flow
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(client, req)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the handler goroutine was not evicted by the write deadline")
	}

	snap := p.Gateway().AgentState.CBDetailedSnapshot()
	circuits, _ := snap["circuits"].(map[string]interface{})
	for key, c := range circuits {
		cm := c.(map[string]interface{})
		if charged(cm["failure_count"]) {
			t.Fatalf("a client stall must not charge the provider, %s = %v", key, cm)
		}
	}

	var buf strings.Builder
	p.Gateway().Metrics.WritePrometheus(&buf)
	if !strings.Contains(buf.String(), `nenya_client_stalled_total{model="test-model", provider="test-provider-0"} 1`) {
		t.Fatalf("expected the client_stalled telemetry, got:\n%s", buf.String())
	}

	// The upstream must have observed the cancellation (bounded eviction).
	select {
	case <-upstreamDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the upstream dispatch was not canceled within the deadline")
	}
}

// deadlineAwareWriter models a real client connection's deadline semantics:
// SetWriteDeadline stores the instant, and a Write issued at or after it fails
// with a timeout. It never blocks — the flip side of stuckClientWriter — so a
// slow-but-alive stream is the only variable.
type deadlineAwareWriter struct {
	header   http.Header
	mu       sync.Mutex
	deadline time.Time
}

func (d *deadlineAwareWriter) Header() http.Header { return d.header }

func (d *deadlineAwareWriter) WriteHeader(code int) {}

func (d *deadlineAwareWriter) Write(p []byte) (int, error) {
	d.mu.Lock()
	deadline := d.deadline
	d.mu.Unlock()
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return 0, &fakeTimeoutError{}
	}
	return len(p), nil
}

func (d *deadlineAwareWriter) Flush() {}

func (d *deadlineAwareWriter) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	d.deadline = t
	d.mu.Unlock()
	return nil
}

// TestSlowButAliveProviderUnaffected pins the flip side: arming the deadline
// per write must not trip on idle gaps — a provider that dribbles chunks
// slower than the deadline still streams to completion, because each write
// re-arms before it fires. Discriminating against an arm-once implementation:
// the stream-start deadline would be expired by the first 1.2s gap and the
// write would fail.
func TestSlowButAliveProviderUnaffected(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := strings.Repeat("y", 32*1024)
		for i := 0; i < 4; i++ {
			// A gap LONGER than the 1s per-write deadline between chunks.
			time.Sleep(1200 * time.Millisecond)
			if _, err := w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"" + chunk + "\"}}]}\n\n")); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer up.Close()

	p := newStallTimeoutProxy(t, up.URL, 1)
	client := &deadlineAwareWriter{header: make(http.Header)}
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`)
	p.ServeHTTP(client, req)

	// The completion signal is the upstream handler finishing without a
	// write error; assert via the absence of the client_stalled sample (a
	// deadline trip would have recorded it).
	var buf strings.Builder
	p.Gateway().Metrics.WritePrometheus(&buf)
	if strings.Contains(buf.String(), `nenya_client_stalled_total{`) {
		t.Fatalf("a slow provider must never be classified client_stalled, got:\n%s", buf.String())
	}
}

// TestResolveStreamStallTimeout pins the knob matrix: nil gateway or nil
// config disables (defaults force 60s at load), an explicit 0 disables, a
// positive value is seconds.
func TestResolveStreamStallTimeout(t *testing.T) {
	if got := resolveStreamStallTimeout(nil); got != 0 {
		t.Fatalf("resolveStreamStallTimeout(nil) = %v, want 0", got)
	}
	gw := newTestGateway(nil, nil)
	if got := resolveStreamStallTimeout(gw); got != 0 {
		t.Fatalf("nil config = %v, want 0", got)
	}
	gw.Config.Server.StreamStallTimeoutSeconds = config.PtrTo(0)
	if got := resolveStreamStallTimeout(gw); got != 0 {
		t.Fatalf("explicit zero = %v, want 0", got)
	}
	gw.Config.Server.StreamStallTimeoutSeconds = config.PtrTo(45)
	if got := resolveStreamStallTimeout(gw); got != 45*time.Second {
		t.Fatalf("explicit 45 = %v, want 45s", got)
	}
}
