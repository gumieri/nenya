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

func newEarlyErrorProxy(t *testing.T, upstreamURLs []string, enableEarlyFailover bool) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(60)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(100000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Governance.EmptyStreamAsError = config.PtrTo(true)
	cfg.Governance.EarlyStreamErrorFailover = config.PtrTo(enableEarlyFailover)

	cfg.Providers = make(map[string]config.ProviderConfig)
	cfg.Agents = make(map[string]config.AgentConfig)

	for i, url := range upstreamURLs {
		pname := fmt.Sprintf("test-provider-%d", i)
		cfg.Providers[pname] = config.ProviderConfig{
			URL:       url + "/v1/chat/completions",
			AuthStyle: "none",
		}
	}

	// Build agent with multiple providers in fallback chain
	models := make([]config.AgentModel, len(upstreamURLs))
	for i := range upstreamURLs {
		pname := fmt.Sprintf("test-provider-%d", i)
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

func makeTestRequest(t *testing.T) *http.Request {
	t.Helper()
	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`
	return testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
}

func runRequest(t *testing.T, p *Proxy) *httptest.ResponseRecorder {
	t.Helper()
	req := makeTestRequest(t)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// TestEarlyStreamErrorFailover verifies that when the FIRST upstream returns a
// 200 stream whose first SSE event is an error, and a second target exists,
// the proxy fails over to the second target and the client receives content.
func TestEarlyStreamErrorFailover(t *testing.T) {
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32

	// First upstream: 200 with immediate error event
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		// The Nvidia/aggregator style flat error
		_, _ = fmt.Fprintf(w, "data: {\"message\":\"Streaming response failed: [502] Upstream error from Nvidia: Service temporarily overloaded\",\"type\":\"server_error\"}\n\n")
	}))
	defer up1.Close()

	// Second upstream: normal content + DONE
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer up2.Close()

	p := newEarlyErrorProxy(t, []string{up1.URL, up2.URL}, true)
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	respBody := rec.Body.String()

	if !strings.Contains(respBody, "ok") {
		t.Errorf("expected second target's content, got: %s", respBody)
	}
	if strings.Contains(respBody, "server_error") || strings.Contains(respBody, "Nvidia") {
		t.Errorf("client should not see the first target's error, got: %s", respBody)
	}

	// Exactly 2 upstream calls: first fails over, second succeeds
	if got := firstCalls.Load(); got != 1 {
		t.Fatalf("expected 1 call to first target, got %d", got)
	}
	if got := secondCalls.Load(); got != 1 {
		t.Fatalf("expected 1 call to second target, got %d", got)
	}

	// Metrics
	var buf strings.Builder
	gw := p.Gateway()
	gw.Metrics.WritePrometheus(&buf)
	metricsOut := buf.String()
	if !strings.Contains(metricsOut, `nenya_stream_early_errors_total{model="test-model", outcome="failover", provider="test-provider-0"} 1`) {
		t.Errorf("expected failover metric, got: %s", metricsOut)
	}
}

// TestEarlyStreamErrorLastTargetForwards verifies that when the ONLY target
// returns an early error event, the proxy forwards it to the client (cannot fail over).
func TestEarlyStreamErrorLastTargetForwards(t *testing.T) {
	var firstCalls atomic.Int32

	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"message\":\"Streaming response failed: [502] Upstream error from Nvidia: Service temporarily overloaded\",\"type\":\"server_error\"}\n\n")
	}))
	defer up1.Close()

	p := newEarlyErrorProxy(t, []string{up1.URL}, true)
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	respBody := rec.Body.String()

	// Client sees the error event (forwarded)
	if !strings.Contains(respBody, "server_error") || !strings.Contains(respBody, "Nvidia") {
		t.Errorf("client should see the upstream error when no alternate target, got: %s", respBody)
	}

	if got := firstCalls.Load(); got != 1 {
		t.Fatalf("expected 1 call, got %d", got)
	}

	// Metric
	var buf strings.Builder
	gw := p.Gateway()
	gw.Metrics.WritePrometheus(&buf)
	metricsOut := buf.String()
	if !strings.Contains(metricsOut, `outcome="forwarded_last_target"`) {
		t.Errorf("expected forwarded_last_target metric, got: %s", metricsOut)
	}
}

// TestEarlyStreamErrorDisabled verifies that with early_stream_error_failover=false
// the error is forwarded even with multiple targets (no failover).
func TestEarlyStreamErrorDisabled(t *testing.T) {
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32

	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"message\":\"boom\",\"type\":\"server_error\"}\n\n")
	}))
	defer up1.Close()

	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer up2.Close()

	p := newEarlyErrorProxy(t, []string{up1.URL, up2.URL}, false)
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	respBody := rec.Body.String()

	// Error is forwarded, no failover
	if !strings.Contains(respBody, "server_error") {
		t.Errorf("expected error forwarded when config disabled, got: %s", respBody)
	}
	if got := firstCalls.Load(); got != 1 {
		t.Fatalf("expected 1 call to first, got %d", got)
	}
	if got := secondCalls.Load(); got != 0 {
		t.Fatalf("expected 0 calls to second when disabled, got %d", got)
	}
}

// TestNormalStreamUnaffected verifies the probe doesn't break normal streams.
func TestNormalStreamUnaffected(t *testing.T) {
	var calls atomic.Int32

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello \"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	p := newEarlyErrorProxy(t, []string{up.URL}, true)
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	respBody := rec.Body.String()

	if !strings.Contains(respBody, "Hello ") || !strings.Contains(respBody, "world") {
		t.Errorf("normal stream broken, got: %s", respBody)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 call, got %d", got)
	}
}

// TestEmptyStreamStillWorks verifies the old empty_stream_as_error behavior is preserved.
func TestEmptyStreamStillWorks(t *testing.T) {
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32

	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		// Empty body, just headers
	}))
	defer up1.Close()

	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer up2.Close()

	p := newEarlyErrorProxy(t, []string{up1.URL, up2.URL}, true)
	rec := runRequest(t, p)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	respBody := rec.Body.String()

	if !strings.Contains(respBody, "ok") {
		t.Errorf("empty stream should fail over, got: %s", respBody)
	}
	if got := firstCalls.Load(); got != 1 {
		t.Fatalf("expected 1 call to first, got %d", got)
	}
	if got := secondCalls.Load(); got != 1 {
		t.Fatalf("expected 1 call to second, got %d", got)
	}

	var buf strings.Builder
	gw := p.Gateway()
	gw.Metrics.WritePrometheus(&buf)
	if !strings.Contains(buf.String(), `nenya_empty_stream_total{model="test-model", provider="test-provider-0"} 1`) {
		t.Errorf("expected empty_stream_total metric")
	}
}
