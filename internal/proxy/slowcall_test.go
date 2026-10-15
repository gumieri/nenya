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
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/resilience"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/testutil"
)

// NENYA-153 — a call slower than the agent's threshold counts as a
// circuit-breaker failure while the client is still served, so a provider that
// answers but takes minutes stops pooling traffic on a degraded target.

// newSlowCallProxy wires an agent whose slow-call threshold is
// `thresholdSeconds`, pointing at upstreamURL. The gateway's breaker uses the
// package default consecutive-failure threshold (see newBreakerGateway for the
// threshold-1 breaker used to assert the open transition).
func newSlowCallProxy(t *testing.T, upstreamURL string, thresholdSeconds int) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(60)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(100000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Governance.SlowCallThresholdSeconds = config.PtrTo(thresholdSeconds)
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider": {
			URL:       upstreamURL + "/v1/chat/completions",
			AuthStyle: "none",
		},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy:                 "fallback",
			SlowCallThresholdSeconds: config.PtrTo(thresholdSeconds),
			Models: []config.AgentModel{
				{Provider: "test-provider", Model: "test-model"},
			},
		},
	}
	gw := gateway.New(context.Background(), *cfg, &config.SecretsConfig{ClientToken: "test-token"}, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

func TestSlowCallSignal(t *testing.T) {
	if newSlowCallSignal(0) != nil {
		t.Error("a disabled threshold must not allocate a signal")
	}
	if newSlowCallSignal(-time.Second) != nil {
		t.Error("a negative threshold must not allocate a signal")
	}

	var nilSignal *slowCallSignal
	nilSignal.observe(time.Hour)
	if nilSignal.slow() {
		t.Error("a nil signal must never report slow")
	}

	s := newSlowCallSignal(2 * time.Second)
	if s.slow() {
		t.Error("an unobserved signal must not report slow")
	}
	s.observe(3 * time.Second)
	if !s.slow() {
		t.Error("an over-threshold first output must report slow")
	}
	// Only the first observation counts: a continuation re-dispatch must not
	// overwrite the logical request's first token.
	s.observe(100 * time.Millisecond)
	if !s.slow() || s.elapsed != 3*time.Second {
		t.Fatalf("a later observation overwrote the first: elapsed=%v slow=%v", s.elapsed, s.slow())
	}

	fast := newSlowCallSignal(2 * time.Second)
	fast.observe(2 * time.Second)
	if fast.slow() {
		t.Error("a call exactly at the threshold is not slow (strictly greater)")
	}
}

// TestSlowCallTripsBreakerWithoutFailingTheClient pins the core contract: a
// stream whose first token exceeds the threshold opens the breaker while the
// client still receives the full answer.
func TestSlowCallTripsBreakerWithoutFailingTheClient(t *testing.T) {
	const firstTokenDelay = 1100 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(firstTokenDelay)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	p := newSlowCallProxy(t, upstream.URL, 1)
	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body))

	// The client is served in full: the slow call is not an error.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("client did not receive the answer: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "gateway_error") {
		t.Fatalf("a slow call must not surface as an error: %s", rec.Body.String())
	}

	// The breaker saw a failure. The gateway's breaker uses the package default
	// consecutive-failure threshold, so assert the charged failure rather than
	// the open state (TestSlowCallTripsBreakerOpenState covers the transition).
	gw := p.Gateway()
	if got := getFailureCountFromSnapshot(gw.AgentState.CBDetailedSnapshot(), "test-agent:test-provider:test-model"); got != 1 {
		t.Fatalf("breaker consecutive failures = %d, want 1", got)
	}

	var metrics strings.Builder
	gw.Metrics.WritePrometheus(&metrics)
	if !strings.Contains(metrics.String(), `nenya_slow_calls_total{agent="test-agent", model="test-model", provider="test-provider"} 1`) {
		t.Fatalf("slow-call metric missing: %s", metrics.String())
	}
}

// newBreakerGateway builds a minimal gateway whose breaker opens on a single
// failure, so the slow-call rules can be asserted without five slow calls (the
// gateway's own breaker uses the package default threshold).
func newBreakerGateway(t *testing.T, thresholdSeconds int, cooldown time.Duration) (*gateway.NenyaGateway, routing.UpstreamTarget) {
	t.Helper()
	cfg := *testutil.MinimalConfig()
	cfg.Governance.SlowCallThresholdSeconds = config.PtrTo(thresholdSeconds)
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {SlowCallThresholdSeconds: config.PtrTo(thresholdSeconds)},
	}
	gw := &gateway.NenyaGateway{
		Config: cfg,
		AgentState: &routing.AgentState{
			Counters: make(map[string]uint64),
			CB:       resilience.NewCircuitBreaker(1, 1, 1, cooldown, nil),
		},
		Logger: slog.Default(),
	}
	return gw, routing.UpstreamTarget{
		CoolKey: "test-agent:test-provider:test-model", Model: "test-model", Provider: "test-provider",
	}
}

// TestSlowCallTripsBreakerOpenState pins the transition the issue names: a
// sustained slow target opens the breaker.
func TestSlowCallTripsBreakerOpenState(t *testing.T) {
	gw, target := newBreakerGateway(t, 2, time.Minute)

	// A call at the threshold is not slow, so the breaker stays closed.
	if recordSlowCall(gw, target, "test-agent", 2*time.Second, time.Minute) {
		t.Fatal("a call at the threshold must not be charged")
	}
	if got := gw.AgentState.CBSnapshot()[target.CoolKey]; got == "open" {
		t.Fatalf("a fast call opened the breaker: %v", got)
	}

	if !recordSlowCall(gw, target, "test-agent", 3*time.Second, time.Minute) {
		t.Fatal("an over-threshold call was not charged to the breaker")
	}
	if got := gw.AgentState.CBSnapshot()[target.CoolKey]; got != "open" {
		t.Fatalf("circuit state = %q, want open", got)
	}
}

// TestFastCallDoesNotTripBreaker is the negative control: a prompt first token
// leaves the breaker closed and records no slow call.
func TestFastCallDoesNotTripBreaker(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	p := newSlowCallProxy(t, upstream.URL, 5)
	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	gw := p.Gateway()
	if states := gw.AgentState.CBSnapshot(); states["test-agent:test-provider:test-model"] == "open" {
		t.Fatalf("a fast call opened the breaker: %v", states)
	}
	var metrics strings.Builder
	gw.Metrics.WritePrometheus(&metrics)
	// The counter's HELP/TYPE lines are always emitted; only a sample line means
	// a slow call was actually counted.
	if strings.Contains(metrics.String(), `nenya_slow_calls_total{model=`) {
		t.Fatalf("a fast call recorded a slow call: %s", metrics.String())
	}
}

// TestDisabledThresholdNeverCountsSlowCalls pins the off switch.
func TestDisabledThresholdNeverCountsSlowCalls(t *testing.T) {
	// The upstream is genuinely slow: only the disabled threshold can explain
	// the absence of a charge.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(1100 * time.Millisecond)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	p := newSlowCallProxy(t, upstream.URL, 0)
	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	gw := p.Gateway()
	if got := getFailureCountFromSnapshot(gw.AgentState.CBDetailedSnapshot(), "test-agent:test-provider:test-model"); got != 0 {
		t.Fatalf("a disabled threshold charged %d failures, want 0", got)
	}
	var metrics strings.Builder
	gw.Metrics.WritePrometheus(&metrics)
	if strings.Contains(metrics.String(), `nenya_slow_calls_total{model=`) {
		t.Fatalf("a disabled threshold recorded a slow call: %s", metrics.String())
	}
}

// TestSlowCallDoesNotCloseHalfOpenCircuit pins the half-open rule: a slow probe
// counts as a failure, so the circuit re-opens instead of closing on it.
func TestSlowCallDoesNotCloseHalfOpenCircuit(t *testing.T) {
	// Long enough that the circuit cannot expire between the charge and the
	// snapshot (CBSnapshot reports an expired Open circuit as half_open).
	const cooldown = 2 * time.Second
	gw, target := newBreakerGateway(t, 2, cooldown)

	// Open the circuit with a slow call, then let the cooldown lapse so the next
	// Allow takes the half-open probe slot.
	if !recordSlowCall(gw, target, "test-agent", 3*time.Second, cooldown) {
		t.Fatal("recordSlowCall did not count an over-threshold call")
	}
	if got := gw.AgentState.CBSnapshot()[target.CoolKey]; got != "open" {
		t.Fatalf("circuit state = %q, want open", got)
	}
	time.Sleep(cooldown + 50*time.Millisecond)
	if !gw.AgentState.CB.Allow(target.CoolKey) {
		t.Fatal("expected the half-open probe to be allowed after the cooldown lapsed")
	}

	// The probe is slow: it must re-open, never close.
	if !recordSlowCall(gw, target, "test-agent", 3*time.Second, cooldown) {
		t.Fatal("a slow half-open probe was not charged")
	}
	if got := gw.AgentState.CBSnapshot()[target.CoolKey]; got != "open" {
		t.Fatalf("a slow half-open probe left the circuit %q, want open", got)
	}
}

// TestSlowCallIgnoresUnmeasuredAndUnknownTargets pins the no-op cases.
func TestSlowCallIgnoresUnmeasuredAndUnknownTargets(t *testing.T) {
	p := newSlowCallProxy(t, "http://127.0.0.1:1", 1)
	gw := p.Gateway()
	target := routing.UpstreamTarget{CoolKey: "test-agent:test-provider:test-model", Model: "test-model", Provider: "test-provider"}

	if recordSlowCall(gw, target, "test-agent", 0, time.Minute) {
		t.Error("an unmeasured (zero) elapsed time must not count")
	}
	if recordSlowCall(gw, target, "test-agent", time.Second, time.Minute) {
		t.Error("a call exactly at the threshold must not count")
	}
	noKey := routing.UpstreamTarget{Model: "m", Provider: "p"}
	if recordSlowCall(gw, noKey, "test-agent", time.Hour, time.Minute) {
		t.Error("a target without a breaker key must not count")
	}
	if recordSlowCall(nil, target, "test-agent", time.Hour, time.Minute) {
		t.Error("a nil gateway must not count")
	}
	if states := gw.AgentState.CBSnapshot(); states[target.CoolKey] == "open" {
		t.Fatalf("a no-op opened the breaker: %v", states)
	}
}

// TestBufferedSlowCallChargesFailureAndServesClient pins the buffered path: the
// whole call (dispatch to body read) is the clock, the client still gets the
// answer, and the breaker is charged.
func TestBufferedSlowCallChargesFailureAndServesClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		time.Sleep(1100 * time.Millisecond)
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	p := newSlowCallProxy(t, upstream.URL, 1)
	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hi") {
		t.Fatalf("client did not receive the answer: %s", rec.Body.String())
	}
	gw := p.Gateway()
	if got := getFailureCountFromSnapshot(gw.AgentState.CBDetailedSnapshot(), "test-agent:test-provider:test-model"); got != 1 {
		t.Fatalf("breaker consecutive failures = %d, want 1", got)
	}
}

// TestBufferedFastCallRecordsSuccess pins the recovery half of the buffered
// path: a served fast call must count as a breaker success, or a half-open
// circuit would burn its probe slots on buffered-only traffic and never close.
func TestBufferedFastCallRecordsSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	p := newSlowCallProxy(t, upstream.URL, 5)
	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	detail := p.Gateway().AgentState.CBDetailedSnapshot()
	if got := getFailureCountFromSnapshot(detail, "test-agent:test-provider:test-model"); got != 0 {
		t.Fatalf("a fast buffered call charged %d failures, want 0", got)
	}
	circuits, _ := detail["circuits"].(map[string]interface{})
	entry, _ := circuits["test-agent:test-provider:test-model"].(map[string]interface{})
	if count, _ := entry["success_count"].(uint32); count != 1 {
		t.Fatalf("a fast buffered call recorded %v successes, want 1", entry["success_count"])
	}
}

// TestHalfOpenRecoveryViaBufferedSuccess pins the ticket's recovery requirement
// end to end at the breaker level: a slow call opens the circuit, and a fast
// buffered probe closes it again.
func TestHalfOpenRecoveryViaBufferedSuccess(t *testing.T) {
	const cooldown = 2 * time.Second
	gw, target := newBreakerGateway(t, 2, cooldown)

	if !recordSlowCall(gw, target, "test-agent", 3*time.Second, cooldown) {
		t.Fatal("the slow call was not charged")
	}
	time.Sleep(cooldown + 50*time.Millisecond)
	if !gw.AgentState.CB.Allow(target.CoolKey) {
		t.Fatal("expected the half-open probe to be allowed")
	}

	// A fast buffered probe closes the circuit (a dispatch instant in the past
	// is a fast call).
	recordBufferedOutcome(gw, target, "test-agent", time.Now().Add(-time.Millisecond), time.Now(), cooldown)
	if got := gw.AgentState.CBSnapshot()[target.CoolKey]; got != "closed" {
		t.Fatalf("circuit state = %q, want closed after a fast probe", got)
	}
}

// TestSlowBufferedProbeReopensHalfOpenCircuit is the mirror: a slow probe on the
// buffered path must re-open, never close (the decision precedes the success).
func TestSlowBufferedProbeReopensHalfOpenCircuit(t *testing.T) {
	const cooldown = 2 * time.Second
	gw, target := newBreakerGateway(t, 2, cooldown)

	if !recordSlowCall(gw, target, "test-agent", 3*time.Second, cooldown) {
		t.Fatal("the slow call was not charged")
	}
	time.Sleep(cooldown + 50*time.Millisecond)
	if !gw.AgentState.CB.Allow(target.CoolKey) {
		t.Fatal("expected the half-open probe to be allowed")
	}

	recordBufferedOutcome(gw, target, "test-agent", time.Now().Add(-3*time.Second), time.Now(), cooldown)
	if got := gw.AgentState.CBSnapshot()[target.CoolKey]; got != "open" {
		t.Fatalf("circuit state = %q, want open after a slow probe", got)
	}
}

// TestSlowCallThresholdFor pins the resolution the proxy actually uses: the
// per-agent override wins over the global value, an unknown agent falls back to
// the global, and an unconfigured governance section is off.
func TestSlowCallThresholdFor(t *testing.T) {
	cfg := *testutil.MinimalConfig()
	cfg.Governance.SlowCallThresholdSeconds = config.PtrTo(5)
	cfg.Agents = map[string]config.AgentConfig{
		"overridden": {SlowCallThresholdSeconds: config.PtrTo(1)},
		"disabled":   {SlowCallThresholdSeconds: config.PtrTo(0)},
	}
	gw := &gateway.NenyaGateway{Config: cfg}

	tests := []struct {
		name  string
		agent string
		want  time.Duration
	}{
		{name: "agent override wins", agent: "overridden", want: time.Second},
		{name: "agent zero disables the global", agent: "disabled", want: 0},
		{name: "unknown agent inherits the global", agent: "absent", want: 5 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := slowCallThresholdFor(gw, tt.agent); got != tt.want {
				t.Fatalf("slowCallThresholdFor(%q) = %v, want %v", tt.agent, got, tt.want)
			}
		})
	}

	off := *testutil.MinimalConfig()
	off.Governance.SlowCallThresholdSeconds = nil
	if got := slowCallThresholdFor(&gateway.NenyaGateway{Config: off}, "absent"); got != 0 {
		t.Fatalf("unconfigured threshold = %v, want off", got)
	}
	if got := slowCallThresholdFor(nil, "absent"); got != 0 {
		t.Fatalf("nil gateway threshold = %v, want off", got)
	}
}

// TestSlowCancelDoesNotCloseHalfOpenCircuit pins NENYA-42 against NENYA-153: a
// client cancel is connection-scoped (no state write), but it must not become a
// success that closes a half-open circuit on a call whose first token was
// already judged slow.
func TestSlowCancelDoesNotCloseHalfOpenCircuit(t *testing.T) {
	const cooldown = 2 * time.Second
	gw, target := newBreakerGateway(t, 2, cooldown)
	if !recordSlowCall(gw, target, "test-agent", 3*time.Second, cooldown) {
		t.Fatal("the slow call was not charged")
	}
	time.Sleep(cooldown + 50*time.Millisecond)
	if !gw.AgentState.CB.Allow(target.CoolKey) {
		t.Fatal("expected the half-open probe to be allowed")
	}

	sig := newSlowCallSignal(2 * time.Second)
	sig.observe(3 * time.Second)
	recordStreamResult(gw, streamOutcome{
		target:      target,
		agentName:   "test-agent",
		copyErr:     context.Canceled,
		slowCall:    sig,
		upstreamErr: &upstreamErrorState{},
	})
	if got := gw.AgentState.CBSnapshot()[target.CoolKey]; got != "half_open" {
		t.Fatalf("circuit state = %q, want half_open (a cancel releases the probe, it records no outcome)", got)
	}
	if !gw.AgentState.CB.Allow(target.CoolKey) {
		t.Fatal("the released probe slot must be usable again")
	}
}

// TestUpstreamErrorEventVerdictPreventsSuccess pins the in-stream error verdict:
// the observer charges the failure while the frame is parsed, and the terminal
// outcome must not clear it with a success (which would also close a half-open
// circuit on a stream the client saw fail).
func TestUpstreamErrorEventVerdictPreventsSuccess(t *testing.T) {
	gw, target := newBreakerGateway(t, 5, time.Minute)
	recordStreamResult(gw, streamOutcome{
		target:      target,
		agentName:   "test-agent",
		upstreamErr: &upstreamErrorState{seen: true},
	})
	detail := gw.AgentState.CBDetailedSnapshot()
	circuits, _ := detail["circuits"].(map[string]interface{})
	if _, present := circuits[target.CoolKey]; present {
		t.Fatalf("a stream with an upstream error event recorded a terminal outcome: %v", circuits[target.CoolKey])
	}
}

// TestStreamWithUpstreamErrorEventDoesNotRecordSuccess is the end-to-end pin: a
// 200 stream carrying an error frame is a failed attempt, not a success.
func TestStreamWithUpstreamErrorEventDoesNotRecordSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"error\",\"code\":\"server_is_overloaded\"}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	p := newSlowCallProxy(t, upstream.URL, 0)
	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body))

	gw := p.Gateway()
	if got := getFailureCountFromSnapshot(gw.AgentState.CBDetailedSnapshot(), "test-agent:test-provider:test-model"); got != 1 {
		t.Fatalf("breaker consecutive failures = %d, want 1 (the in-stream error must survive the terminal outcome)", got)
	}
}

// TestBufferStreamResponseFlagsInStreamError pins the buffered MCP predicate
// (NENYA-153): the same rejection shapes the bootstrap head probe rejects must
// flag the buffer, and a normal content frame must not.
func TestBufferStreamResponseFlagsInStreamError(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{name: "nested error object", payload: `{"error":{"message":"server_is_overloaded"}}`, want: true},
		{name: "typed error event", payload: `{"type":"error","code":"server_is_overloaded"}`, want: true},
		{name: "response.failed", payload: `{"type":"response.failed"}`, want: true},
		{name: "content delta", payload: `{"choices":[{"delta":{"content":"hi"}}]}`},
		{name: "finish chunk", payload: `{"choices":[{"delta":{},"finish_reason":"stop"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := "data: " + tt.payload + "\n\ndata: [DONE]\n\n"
			buf, err := bufferStreamResponse(context.Background(), strings.NewReader(body), slog.Default(), "test-model", routing.UpstreamTarget{})
			if err != nil {
				t.Fatalf("bufferStreamResponse: %v", err)
			}
			if buf.streamError != tt.want {
				t.Fatalf("streamError = %v, want %v for %s", buf.streamError, tt.want, tt.payload)
			}
		})
	}
}

// TestBufferedStreamOutcomes pins the MCP buffered path's breaker outcome
// (NENYA-153): a served buffer records a success, an in-stream error frame
// records a failure, and neither is recorded twice.
func TestBufferedStreamOutcomes(t *testing.T) {
	tests := []struct {
		name        string
		sse         string
		wantFailure uint32
		wantSuccess uint32
	}{
		{
			name:        "served content records a success",
			sse:         "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
			wantSuccess: 1,
		},
		{
			name:        "in-stream error records a failure",
			sse:         "data: {\"type\":\"error\",\"code\":\"server_is_overloaded\"}\n\ndata: [DONE]\n\n",
			wantFailure: 1,
		},
		{
			// NENYA-168: a capacity rejection that is not shaped as an error
			// envelope is a failed attempt too.
			name:        "non-envelope capacity rejection records a failure",
			sse:         "data: {\"message\":\"model is at capacity\"}\n\ndata: [DONE]\n\n",
			wantFailure: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newSlowCallProxy(t, "http://127.0.0.1:1", 0)
			gw := p.Gateway()
			target := routing.UpstreamTarget{
				CoolKey: "test-agent:test-provider:test-model", Model: "test-model", Provider: "test-provider",
			}
			action := upstreamAction{
				kind:       actionStream,
				resp:       &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tt.sse))},
				dispatchAt: time.Now(),
				cancel:     func() {},
			}

			if _, err := p.handleBufferedStream(context.Background(), action, target, gw, time.Minute, "test-agent", pipeline.CanarResult{}); err != nil {
				t.Fatalf("handleBufferedStream: %v", err)
			}

			detail := gw.AgentState.CBDetailedSnapshot()
			if got := getFailureCountFromSnapshot(detail, target.CoolKey); got != tt.wantFailure {
				t.Fatalf("failure count = %d, want %d", got, tt.wantFailure)
			}
			circuits, _ := detail["circuits"].(map[string]interface{})
			entry, _ := circuits[target.CoolKey].(map[string]interface{})
			if count, _ := entry["success_count"].(uint32); count != tt.wantSuccess {
				t.Fatalf("success count = %v, want %d", entry["success_count"], tt.wantSuccess)
			}
		})
	}
}
