package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/nenya/internal/gateway"
)

// probeCtxKey marks the request context in the test below.
type probeCtxKey struct{}

// ctxProbeHandler records whether Enabled received a context carrying the
// probe marker. It never emits records; the level check is the only probe.
type ctxProbeHandler struct {
	mu        sync.Mutex
	sawMarker bool
}

func (h *ctxProbeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	if ctx.Value(probeCtxKey{}) != nil {
		h.mu.Lock()
		h.sawMarker = true
		h.mu.Unlock()
	}
	return false
}

func (h *ctxProbeHandler) Handle(context.Context, slog.Record) error { return nil }
func (h *ctxProbeHandler) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h *ctxProbeHandler) WithGroup(string) slog.Handler             { return h }

// TestSessionHeaderLevelCheckUsesRequestContext pins AUD-25 (NENYA-233):
// the mid-request logger level check must probe the REQUEST context, not
// context.Background(). Standard handlers ignore the context, so the fix is
// invisible with them — the probe handler is what makes the request-context
// threading observable: a trace-aware or request-scoped filtering handler
// can only make per-request decisions if it is handed the request's
// context.
func TestSessionHeaderLevelCheckUsesRequestContext(t *testing.T) {
	h := &ctxProbeHandler{}
	gw := &gateway.NenyaGateway{Logger: slog.New(h)}

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r = r.WithContext(context.WithValue(r.Context(), probeCtxKey{}, struct{}{}))

	// Seed the memoized session key so the function synthesizes a session ID
	// (the only path that reaches the level check) without building routing.
	req := &chatRequest{ModelName: "test-model"}
	req.sessionKeyDone = true
	req.sessionKey = "0123456789abcdef"
	req.sessionKeyOK = true

	ensureOpencodeSessionHeader(gw, r, req)

	h.mu.Lock()
	saw := h.sawMarker
	h.mu.Unlock()
	if !saw {
		t.Fatal("the mid-request level check did not receive the request context: context.Background() is still in use (AUD-25)")
	}
}
