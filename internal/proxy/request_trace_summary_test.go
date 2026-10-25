package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nenya/internal/testutil"
)

// traceProbe captures emitted log messages so a test can assert on the
// request-trace summary.
type traceProbe struct {
	mu   sync.Mutex
	msgs []string
}

func (h *traceProbe) Enabled(context.Context, slog.Level) bool { return true }
func (h *traceProbe) Handle(_ context.Context, rec slog.Record) error {
	h.mu.Lock()
	h.msgs = append(h.msgs, rec.Message)
	h.mu.Unlock()
	return nil
}
func (h *traceProbe) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *traceProbe) WithGroup(string) slog.Handler      { return h }

func (h *traceProbe) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.msgs {
		if m == msg {
			n++
		}
	}
	return n
}

// TestRequestTraceSummaryOnlyForDispatchBearing pins AUD-26 (NENYA-234): the
// Info "request trace" summary covers dispatch-bearing requests only. The
// code claimed the defer was "registered after validateChatRequest so
// body-read failures and the cache-hit sentinel do not emit a misleading
// summary" — but it was registered BEFORE validation, so every body-read
// failure emitted a zero-stage, zero-token summary. Post-fix, a validation
// failure emits none and a validated request still emits one.
func TestRequestTraceSummaryOnlyForDispatchBearing(t *testing.T) {
	p, _, _ := newReloadWindowProxy(t)
	gw := p.Gateway()
	probe := &traceProbe{}
	gw.Logger = slog.New(probe)

	// (a) Body-read failure: validation never passes, nothing dispatches —
	// no summary.
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", "{not json"))
	if rec.Code == http.StatusOK {
		t.Fatalf("expected the malformed body to be rejected, got %d", rec.Code)
	}
	if n := probe.count("request trace"); n != 0 {
		t.Fatalf("a body-read failure must not emit a request-trace summary, got %d", n)
	}

	// (b) Validated request: dispatch-bearing — exactly one summary.
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions",
		map[string]any{
			"model":    "test-agent",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		}))
	if n := probe.count("request trace"); n != 1 {
		var sb strings.Builder
		probe.mu.Lock()
		for _, m := range probe.msgs {
			sb.WriteString(m + "\n")
		}
		probe.mu.Unlock()
		t.Fatalf("a dispatch-bearing request must emit exactly one request-trace summary, got %d\nlog:\n%s", n, sb.String())
	}
}
