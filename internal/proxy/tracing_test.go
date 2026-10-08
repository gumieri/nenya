package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/testutil"
	"github.com/nenya/internal/tracing"
)

// fixtureTraceparent is a well-formed sampled W3C traceparent.
const fixtureTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func tracingTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newCaptureUpstreamHeaders starts an SSE upstream recording the
// Traceparent header of each request.
func newCaptureUpstreamHeaders(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("Traceparent"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// TestBuildUpstreamRequestPropagatesCurrentSpan verifies NENYA-140: the
// upstream request carries the CURRENT span's traceparent (same trace, the
// gateway hop as parent), overriding the client's original header.
func TestBuildUpstreamRequestPropagatesCurrentSpan(t *testing.T) {
	cfg := config.Config{Server: config.ServerConfig{UserAgent: "nenya-test/1.0"}}
	providers := map[string]*config.Provider{
		"test": {
			Name:      "test",
			URL:       "https://api.test.com/v1/chat/completions",
			BaseURL:   "https://api.test.com",
			AuthStyle: "none",
		},
	}
	gw := &gateway.NenyaGateway{
		Config:    cfg,
		Secrets:   &config.SecretsConfig{ClientToken: "client-token"},
		Providers: providers,
		Logger:    tracingTestLogger(),
	}
	src := httptest.NewRequest("POST", "http://client/v1/chat/completions", nil)
	// The client's own traceparent must be overridden by the gateway's
	// current span context.
	src.Header.Set("Traceparent", "00-11111111111111111111111111111111-2222222222222222-01")

	ctx, _ := tracing.StartRequest(context.Background(), tracingTestLogger(), fixtureTraceparent, "chat_completions")
	prx := &Proxy{}
	req, err := prx.buildUpstreamRequest(gw, ctx, "POST", "https://api.test.com/v1/chat/completions", []byte(`{}`), "test", "", "", src.Header)
	if err != nil {
		t.Fatalf("buildUpstreamRequest: %v", err)
	}

	got := req.Header.Get("Traceparent")
	want := tracing.OutgoingTraceparent(ctx)
	if got != want {
		t.Fatalf("Traceparent = %q, want the current span context %q", got, want)
	}
	parsed, ok := tracing.ParseTraceparent(got)
	if !ok {
		t.Fatalf("propagated traceparent %q does not parse", got)
	}
	if parsed.TraceID.String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("propagated trace id = %s, want the continued inbound trace", parsed.TraceID)
	}
	if got == fixtureTraceparent {
		t.Fatal("propagated header must carry a fresh span id, not the client's")
	}
}

// TestBuildUpstreamRequestDiscardsTracestateOnRestart verifies W3C §4.2:
// when the gateway roots a fresh trace (invalid inbound traceparent), the
// client's tracestate (correlated with the discarded trace) is dropped.
func TestBuildUpstreamRequestDiscardsTracestateOnRestart(t *testing.T) {
	cfg := config.Config{Server: config.ServerConfig{UserAgent: "nenya-test/1.0"}}
	providers := map[string]*config.Provider{
		"test": {Name: "test", URL: "https://api.test.com/v1/chat/completions", BaseURL: "https://api.test.com", AuthStyle: "none"},
	}
	gw := &gateway.NenyaGateway{
		Config:    cfg,
		Secrets:   &config.SecretsConfig{ClientToken: "client-token"},
		Providers: providers,
		Logger:    tracingTestLogger(),
	}
	src := httptest.NewRequest("POST", "http://client/v1/chat/completions", nil)
	src.Header.Set("Traceparent", "not-a-traceparent") // restart
	src.Header.Set("Tracestate", "vendor=key")

	ctx, _ := tracing.StartRequest(context.Background(), tracingTestLogger(), src.Header.Get("Traceparent"), "chat_completions")
	prx := &Proxy{}
	req, err := prx.buildUpstreamRequest(gw, ctx, "POST", "https://api.test.com/v1/chat/completions", []byte(`{}`), "test", "", "", src.Header)
	if err != nil {
		t.Fatalf("buildUpstreamRequest: %v", err)
	}
	if got := req.Header.Get("Tracestate"); got != "" {
		t.Fatalf("tracestate must be discarded on trace restart, got %q", got)
	}
	if tp := req.Header.Get("Traceparent"); tp == "not-a-traceparent" || tp == "" {
		t.Fatalf("fresh trace must be propagated, got %q", tp)
	}
}

// TestTracingDisabledIsInert verifies governance.tracing.enabled=false:
// no response echo, the client Traceparent passes through verbatim (no
// gateway override), and no trace records are emitted.
func TestTracingDisabledIsInert(t *testing.T) {
	upstream, upstreamHeaders := newCaptureUpstreamHeaders(t)
	p := newChatProxy(t, upstream.URL)
	if gw := p.Gateway(); gw != nil {
		gw.Config.Governance.Tracing = &config.TracingConfig{Enabled: config.PtrTo(false)}
	}

	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", `{"model":"test-agent","messages":[{"role":"user","content":"first question"}]}`)
	req.Header.Set("Traceparent", fixtureTraceparent)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if got := rec.Header().Get("Traceparent"); got != "" {
		t.Fatalf("disabled tracing must not echo traceparent, got %q", got)
	}
	headers := upstreamHeaders()
	if len(headers) != 1 {
		t.Fatalf("expected 1 upstream request, got %d", len(headers))
	}
	// Disabled tracing is fully inert: the passthrough forwards the client
	// header verbatim (no gateway span context to override with).
	if headers[0] != fixtureTraceparent {
		t.Fatalf("disabled tracing must pass the client header through verbatim, got %q", headers[0])
	}
}

// TestHandleChatCompletionsEchoesTraceparent verifies the response echoes
// the active traceparent so clients can correlate, and that the upstream
// receives the propagated (re-parented) context (NENYA-140).
func TestHandleChatCompletionsEchoesTraceparent(t *testing.T) {
	upstream, upstreamHeaders := newCaptureUpstreamHeaders(t)
	p := newChatProxy(t, upstream.URL)

	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", `{"model":"test-agent","messages":[{"role":"user","content":"first question"}]}`)
	req.Header.Set("Traceparent", fixtureTraceparent)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	got := rec.Header().Get("Traceparent")
	parsed, ok := tracing.ParseTraceparent(got)
	if !ok {
		t.Fatalf("response Traceparent %q does not parse", got)
	}
	if parsed.TraceID.String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("echoed trace id = %s, want the continued inbound trace", parsed.TraceID)
	}
	headers := upstreamHeaders()
	if len(headers) != 1 {
		t.Fatalf("expected 1 upstream request, got %d", len(headers))
	}
	upstreamTP := headers[0]
	if upstreamTP == fixtureTraceparent {
		t.Fatal("upstream must receive the re-parented context, not the client header verbatim")
	}
	if up, ok := tracing.ParseTraceparent(upstreamTP); !ok || up.TraceID.String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("upstream traceparent %q must carry the same trace id", upstreamTP)
	}
}
