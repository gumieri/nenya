package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/testutil"
)

// TestStalledAfterFirstTokenTerminalSemantics pins the NENYA-156
// failure-semantics contract end to end for the chat path: a stream that
// delivers one token and then goes silent is bounded by the idle timeout, and
// the termination carries ALL of the semantics — the structured
// gateway_error terminal with error_kind=provider_timeout (Retryable +
// ShouldFailover), [DONE] framing, a target cooldown charge (the provider
// failed to finish), and no cache write.
func TestStalledAfterFirstTokenTerminalSemantics(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// First token delivered, then silence past the idle timeout.
		<-r.Context().Done()
	}))
	defer up.Close()

	p := newCachedStreamProxy(t, up.URL)
	gw := p.Gateway()
	gw.Config.Governance.StreamBootstrapBufferBytes = 0
	gw.Config.Governance.EmptyStreamAsError = config.PtrTo(false)
	gw.Config.Governance.EarlyStreamErrorFailover = config.PtrTo(false)
	gw.Config.Governance.StreamIdleTimeoutSeconds = config.PtrTo(1)

	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(rec, req)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("stalled stream was not bounded by the idle timeout")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "upstream stream stalled") {
		t.Fatalf("expected the stall terminal, got: %s", body)
	}
	if !strings.Contains(body, `"error_kind":"provider_timeout"`) {
		t.Fatalf("expected the structured error_kind=provider_timeout terminal, got: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected [DONE] framing after the stall terminal, got: %s", body)
	}
	if got := gw.ResponseCache.Len(); got != 0 {
		t.Fatalf("stalled stream must never be cached, cache len = %d", got)
	}
	snap := gw.AgentState.CBDetailedSnapshot()
	circuits := snap["circuits"].(map[string]interface{})
	c, ok := circuits["test-agent:test-provider-0:test-model"].(map[string]interface{})
	if !ok {
		t.Fatalf("no circuit for the stalled target: %v", snap)
	}
	fc, ok := c["failure_count"]
	if !ok {
		t.Fatalf("failure_count missing from the circuit snapshot: %v", c)
	}
	if !charged(fc) {
		t.Fatalf("expected the stalled target to be charged a cooldown failure, got %v", c)
	}
}

// TestPipeSSE_StallEmitsTerminal pins the NENYA-156 passthrough delta: a
// stalled /proxy/ SSE stream must not abort silently — the client receives
// the same structured gateway_error terminal (error_kind=provider_timeout)
// and [DONE] framing, and the stall telemetry is recorded.
func TestPipeSSE_StallEmitsTerminal(t *testing.T) {
	gw := newTestGateway(nil, nil)

	// A source that yields one chunk and then goes silent.
	src := newSlowThenSilentReader(t, 20*time.Millisecond)
	w := httptest.NewRecorder()

	proxy := &Proxy{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy.pipeSSE(pipeSSEOpts{
		Ctx:          context.Background(),
		Gw:           gw,
		Logger:       logger,
		Src:          src,
		Dst:          w,
		ProviderName: "test-provider",
		StallTimeout: 1 * time.Second,
	})

	body := w.Body.String()
	if !strings.Contains(body, "first chunk") {
		t.Fatalf("expected the pre-stall chunk to be delivered, got: %s", body)
	}
	if !strings.Contains(body, "upstream stream stalled") {
		t.Fatalf("expected the stall terminal, got: %s", body)
	}
	if !strings.Contains(body, `"error_kind":"provider_timeout"`) {
		t.Fatalf("expected error_kind=provider_timeout on the terminal, got: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected [DONE] framing, got: %s", body)
	}

	var buf strings.Builder
	gw.Metrics.WritePrometheus(&buf)
	// The HELP line is emitted unconditionally; assert the labeled SAMPLE so
	// the telemetry claim is not vacuous. The empty model normalizes to the
	// shared unknown label through the cardinality guard (AUD-66).
	if !strings.Contains(buf.String(), `nenya_stream_stalled_total{model="unknown", provider="test-provider"} 1`) {
		t.Fatalf("expected the stall sample, got:\n%s", buf.String())
	}
}

// slowThenSilentReader yields one chunk after delay, then blocks until Close —
// the stall reader's timeout must fire while it is blocked, and Close releases
// the blocked read so no goroutine leaks past the test.
type slowThenSilentReader struct {
	delay  time.Duration
	once   bool
	closed chan struct{}
}

func newSlowThenSilentReader(t *testing.T, delay time.Duration) *slowThenSilentReader {
	r := &slowThenSilentReader{delay: delay, closed: make(chan struct{})}
	t.Cleanup(func() { close(r.closed) })
	return r
}

func (r *slowThenSilentReader) Read(p []byte) (int, error) {
	if !r.once {
		r.once = true
		time.Sleep(r.delay)
		return copy(p, "data: first chunk\n\n"), nil
	}
	<-r.closed
	return 0, io.EOF
}
