package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/testutil"
)

// newCachedStreamProxy builds a single-target proxy with the response cache
// enabled and stream continuation disabled (default), so cache behavior is
// attributable to the truncation guard (NENYA-27) alone.
func newCachedStreamProxy(t *testing.T, upstreamURL string) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(60)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(100000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.ResponseCache = config.ResponseCacheConfig{
		Enabled:           config.PtrTo(true),
		MaxEntries:        16,
		MaxEntryBytes:     65536,
		TTLSeconds:        60,
		EvictEverySeconds: 3600,
	}
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider-0": {URL: upstreamURL + "/v1/chat/completions", AuthStyle: "none"},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models:   []config.AgentModel{{Provider: "test-provider-0", Model: "test-model"}},
		},
	}
	secrets := &config.SecretsConfig{ClientToken: "test-token"}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if gw.ResponseCache == nil {
		t.Fatal("response cache must be constructed for this test")
	}
	t.Cleanup(func() { gw.ResponseCache.Stop() })
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

func writeCleanChatStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
	_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
}

// TestTruncatedStreamNotCached pins NENYA-27 hazard (2): a stream that dies
// mid-generation and is sanitized with an injected gateway_error terminal
// must NOT populate the response cache — the next identical request must
// reach the upstream again instead of replaying the dead upstream's partial
// output.
func TestTruncatedStreamNotCached(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if calls.Add(1) == 1 {
			// First call: stream dies mid-generation (no finish_reason, no [DONE]).
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial tr\"}}]}\n\n")
			return
		}
		writeCleanChatStream(w)
	}))
	defer up.Close()

	p := newCachedStreamProxy(t, up.URL)

	rec1 := runRequest(t, p)
	testutil.AssertResponseStatusCode(t, rec1, http.StatusOK)
	if !strings.Contains(rec1.Body.String(), "gateway_error") {
		t.Errorf("truncated stream should surface the injected gateway_error, got: %s", rec1.Body.String())
	}
	if gw := p.Gateway(); gw.ResponseCache.Len() != 0 {
		t.Errorf("truncated stream must not be cached, cache len = %d", gw.ResponseCache.Len())
	}

	// Second identical request must be a cache miss and hit the upstream.
	rec2 := runRequest(t, p)
	testutil.AssertResponseStatusCode(t, rec2, http.StatusOK)
	if !strings.Contains(rec2.Body.String(), "ok") {
		t.Errorf("second request should receive the fresh clean stream, got: %s", rec2.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 upstream calls (truncation must not poison the cache), got %d", got)
	}
}

// TestCleanStreamIsCached is the control for TestTruncatedStreamNotCached:
// a clean stream populates the cache and a second identical request replays
// without an upstream call.
func TestCleanStreamIsCached(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		calls.Add(1)
		writeCleanChatStream(w)
	}))
	defer up.Close()

	p := newCachedStreamProxy(t, up.URL)

	rec1 := runRequest(t, p)
	testutil.AssertResponseStatusCode(t, rec1, http.StatusOK)
	if gw := p.Gateway(); gw.ResponseCache.Len() != 1 {
		t.Fatalf("clean stream must be cached, cache len = %d", gw.ResponseCache.Len())
	}

	rec2 := runRequest(t, p)
	testutil.AssertResponseStatusCode(t, rec2, http.StatusOK)
	if !strings.Contains(rec2.Body.String(), "ok") {
		t.Errorf("replayed stream should carry the cached content, got: %s", rec2.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("second request must be served from cache, got %d upstream calls", got)
	}
}
