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

// newBootstrapProxy builds a two-target (or N-target) fallback proxy with
// stream bootstrap buffering configured. Early-stream-error failover is
// explicitly OFF so every failover observed in these tests is attributable to
// the bootstrap buffer, not the first-event probe.
func newBootstrapProxy(t *testing.T, upstreamURLs []string, govBytes int, providerBytes ...*int) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(60)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(100000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Governance.EmptyStreamAsError = config.PtrTo(true)
	cfg.Governance.EarlyStreamErrorFailover = config.PtrTo(false)
	cfg.Governance.StreamBootstrapBufferBytes = govBytes

	cfg.Providers = make(map[string]config.ProviderConfig)
	cfg.Agents = make(map[string]config.AgentConfig)

	models := make([]config.AgentModel, len(upstreamURLs))
	for i, url := range upstreamURLs {
		pname := fmt.Sprintf("test-provider-%d", i)
		pc := config.ProviderConfig{
			URL:       url + "/v1/chat/completions",
			AuthStyle: "none",
		}
		if i < len(providerBytes) {
			pc.StreamBootstrapBufferBytes = providerBytes[i]
		}
		cfg.Providers[pname] = pc
		models[i] = config.AgentModel{Provider: pname, Model: "test-model"}
	}
	cfg.Agents["test-agent"] = config.AgentConfig{
		Strategy: "fallback",
		Models:   models,
	}

	secrets := &config.SecretsConfig{
		ClientToken: "test-token",
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

// bootstrapHandshakeUpstream emits the Codex/OpenRouter failure family: SSE
// keep-alive and handshake metadata frames, THEN an in-stream rejection.
func bootstrapHandshakeUpstream(calls *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, ": keep-alive\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"error\",\"code\":\"server_is_overloaded\"}\n\n")
	}))
}

func bootstrapHealthyUpstream(calls *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
}

func bootstrapMetrics(t *testing.T, p *Proxy) string {
	t.Helper()
	var buf strings.Builder
	p.Gateway().Metrics.WritePrometheus(&buf)
	return buf.String()
}

// TestBootstrapRejectionFailover is the core NENYA-45 scenario: the rejection
// arrives after handshake metadata, so the first-event probe would miss it.
// The bootstrap buffer holds the handshake, detects the rejection, and fails
// over before the 200 is committed.
func TestBootstrapRejectionFailover(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	up1 := bootstrapHandshakeUpstream(&firstCalls)
	defer up1.Close()
	up2 := bootstrapHealthyUpstream(&secondCalls)
	defer up2.Close()

	p := newBootstrapProxy(t, []string{up1.URL, up2.URL}, 65536)
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	body := rec.Body.String()
	if !strings.Contains(body, "ok") {
		t.Errorf("expected second target's content, got: %s", body)
	}
	if strings.Contains(body, "server_is_overloaded") {
		t.Errorf("client must not see the pre-commit rejection, got: %s", body)
	}
	if got := firstCalls.Load(); got != 1 {
		t.Fatalf("expected 1 call to first target, got %d", got)
	}
	if got := secondCalls.Load(); got != 1 {
		t.Fatalf("expected 1 call to second target, got %d", got)
	}

	metrics := bootstrapMetrics(t, p)
	if !strings.Contains(metrics, `nenya_stream_bootstrap_total{model="test-model", outcome="rejected_failover", provider="test-provider-0"} 1`) {
		t.Errorf("expected rejected_failover metric, got: %s", metrics)
	}
	if !strings.Contains(metrics, `nenya_stream_early_errors_total{model="test-model", outcome="bootstrap_failover", provider="test-provider-0"} 1`) {
		t.Errorf("expected bootstrap_failover early-error metric, got: %s", metrics)
	}
	if !strings.Contains(metrics, "nenya_stream_bootstrap_hold_seconds") {
		t.Errorf("expected bootstrap hold histogram, got: %s", metrics)
	}
}

// TestBootstrapFlushTransparent verifies a healthy stream that opens with
// handshake metadata: the buffer flushes on the first output event and the
// client receives every byte, including the replayed handshake prefix.
func TestBootstrapFlushTransparent(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, ": keep-alive\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	p := newBootstrapProxy(t, []string{up.URL}, 65536)
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	body := rec.Body.String()
	if !strings.Contains(body, "ok") {
		t.Errorf("expected content, got: %s", body)
	}
	if !strings.Contains(body, "response.created") {
		t.Errorf("handshake prefix must be replayed to the client, got: %s", body)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 call, got %d", got)
	}
	if !strings.Contains(bootstrapMetrics(t, p), `nenya_stream_bootstrap_total{model="test-model", outcome="flushed", provider="test-provider-0"} 1`) {
		t.Errorf("expected flushed metric, got: %s", bootstrapMetrics(t, p))
	}
}

// TestBootstrapOverflowDegrades verifies the bounded-buffer contract: a
// handshake larger than the budget degrades to unbuffered streaming instead
// of buffering indefinitely — content still reaches the client. The comment
// block exceeds one read buffer (4KB) so the overflow decision is
// deterministic regardless of TCP chunking.
func TestBootstrapOverflowDegrades(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, ": %s\n\n", strings.Repeat("x", 8192))
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	p := newBootstrapProxy(t, []string{up.URL}, 32)
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("expected degraded stream to deliver content, got: %s", rec.Body.String())
	}
	if !strings.Contains(bootstrapMetrics(t, p), `nenya_stream_bootstrap_total{model="test-model", outcome="overflow", provider="test-provider-0"} 1`) {
		t.Errorf("expected overflow metric, got: %s", bootstrapMetrics(t, p))
	}
}

// TestBootstrapDisabledDefault pins the opt-in: with no bootstrap budget the
// metadata-then-rejection stream is committed as today and no failover runs.
func TestBootstrapDisabledDefault(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	up1 := bootstrapHandshakeUpstream(&firstCalls)
	defer up1.Close()
	up2 := bootstrapHealthyUpstream(&secondCalls)
	defer up2.Close()

	p := newBootstrapProxy(t, []string{up1.URL, up2.URL}, 0)
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "server_is_overloaded") {
		t.Errorf("disabled bootstrap must commit the stream as before, got: %s", rec.Body.String())
	}
	if got := secondCalls.Load(); got != 0 {
		t.Fatalf("expected 0 calls to second target when disabled, got %d", got)
	}
}

// TestBootstrapLastTargetForwards verifies the last-target branch: a
// rejection with no alternate target is forwarded as a committed stream,
// keeping circuit-breaker impact recorded.
func TestBootstrapLastTargetForwards(t *testing.T) {
	var calls atomic.Int32
	up := bootstrapHandshakeUpstream(&calls)
	defer up.Close()

	p := newBootstrapProxy(t, []string{up.URL}, 65536)
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "server_is_overloaded") {
		t.Errorf("last target must forward the rejection, got: %s", rec.Body.String())
	}
	metrics := bootstrapMetrics(t, p)
	if !strings.Contains(metrics, `nenya_stream_bootstrap_total{model="test-model", outcome="rejected_forwarded", provider="test-provider-0"} 1`) {
		t.Errorf("expected rejected_forwarded metric, got: %s", metrics)
	}
	if !strings.Contains(metrics, `nenya_stream_early_errors_total{model="test-model", outcome="bootstrap_forwarded", provider="test-provider-0"} 1`) {
		t.Errorf("expected bootstrap_forwarded early-error metric, got: %s", metrics)
	}
}

// TestBootstrapOutputWinsBeforeError verifies the post-commit contract: once
// real output flushed the buffer, a later in-stream error is delivered to the
// client — no retry, no failover (first bytes are already committed).
func TestBootstrapOutputWinsBeforeError(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{}}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"error\":{\"message\":\"late boom\"}}\n\n")
	}))
	defer up1.Close()
	up2 := bootstrapHealthyUpstream(&secondCalls)
	defer up2.Close()

	p := newBootstrapProxy(t, []string{up1.URL, up2.URL}, 65536)
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	body := rec.Body.String()
	if !strings.Contains(body, "ok") {
		t.Errorf("expected flushed content, got: %s", body)
	}
	if !strings.Contains(body, "late boom") {
		t.Errorf("post-commit error must be forwarded verbatim, got: %s", body)
	}
	if got := secondCalls.Load(); got != 0 {
		t.Fatalf("post-commit error must not fail over, got %d second-target calls", got)
	}
}

// TestBootstrapProviderOverrideDisables verifies the per-provider opt-out: a
// global budget with providers[0].stream_bootstrap_buffer_bytes=0 keeps
// provider-0 unbuffered while other providers inherit the global.
func TestBootstrapProviderOverrideDisables(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	up1 := bootstrapHandshakeUpstream(&firstCalls)
	defer up1.Close()
	up2 := bootstrapHealthyUpstream(&secondCalls)
	defer up2.Close()

	p := newBootstrapProxy(t, []string{up1.URL, up2.URL}, 65536, config.PtrTo(0))
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "server_is_overloaded") {
		t.Errorf("provider override must disable buffering, got: %s", rec.Body.String())
	}
	if got := secondCalls.Load(); got != 0 {
		t.Fatalf("expected no failover with override disabled, got %d", got)
	}
}
