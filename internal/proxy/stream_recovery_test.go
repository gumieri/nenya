package proxy

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/testutil"
)

func TestContinuationCapture_MaxBytes(t *testing.T) {
	c := newContinuationCapture(10)
	if _, err := c.Write([]byte("0123456789")); err != nil {
		t.Fatalf("Write < limit failed: %v", err)
	}
	if c.exceeded {
		t.Fatal("should not be exceeded at limit")
	}
	if _, err := c.Write([]byte("X")); err != nil {
		t.Fatalf("Write at limit+1 failed: %v", err)
	}
	if !c.exceeded {
		t.Fatal("should be exceeded after limit+1")
	}
	if c.buf.Len() != 0 {
		t.Fatalf("buffer should be reset on exceed, got %d bytes", c.buf.Len())
	}
}

func TestContinuationCapture_Reset(t *testing.T) {
	c := newContinuationCapture(100)
	_, _ = c.Write([]byte("hello"))
	c.exceeded = true
	c.reset()
	if c.buf.Len() != 0 {
		t.Fatalf("buffer should be empty after reset, got %d bytes", c.buf.Len())
	}
	if c.exceeded {
		t.Fatal("exceeded should be false after reset")
	}
}

func TestBuildContinuationMessage_ToolCallInFlight(t *testing.T) {
	p := &Proxy{}
	gw := &gateway.NenyaGateway{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: infra.NewMetrics(),
	}

	cont := &streamContinuation{
		capture:         newContinuationCapture(1024),
		includeReasoning: true,
	}
	cont.capture.buf.WriteString("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"add\",\"arguments\":\"{\"}}]}}]}\n\n")

	msg, reason := p.buildContinuationMessage(context.Background(), gw, cont)
	if msg != nil {
		t.Fatalf("expected nil message, got %v", msg)
	}
	if reason != "gave_up_tool_call" {
		t.Fatalf("expected gave_up_tool_call, got %q", reason)
	}
}

func TestBuildContinuationMessage_ContentPartial(t *testing.T) {
	p := &Proxy{}
	gw := &gateway.NenyaGateway{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: infra.NewMetrics(),
	}

	cont := &streamContinuation{
		capture:         newContinuationCapture(1024),
		includeReasoning: true,
	}
	cont.capture.buf.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n")

	msg, reason := p.buildContinuationMessage(context.Background(), gw, cont)
	if msg == nil {
		t.Fatalf("expected message, got nil with reason %q", reason)
	}
	if reason != "" {
		t.Fatalf("expected empty reason, got %q", reason)
	}
	if msg["role"] != "assistant" {
		t.Fatalf("expected role=assistant, got %q", msg["role"])
	}
	if msg["content"] != "hello" {
		t.Fatalf("expected content=hello, got %q", msg["content"])
	}
}

func TestBuildContinuationMessage_CaptureExceededGivesUp(t *testing.T) {
	p := &Proxy{}
	gw := &gateway.NenyaGateway{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: infra.NewMetrics(),
	}

	cont := &streamContinuation{
		capture:         newContinuationCapture(4),
		includeReasoning: true,
	}
	cont.capture.exceeded = true
	cont.capture.buf.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"way too long\"}}]}\n\n")

	msg, reason := p.buildContinuationMessage(context.Background(), gw, cont)
	if msg != nil {
		t.Fatalf("expected nil message, got %v", msg)
	}
	if reason != "gave_up_no_content" {
		t.Fatalf("expected gave_up_no_content, got %q", reason)
	}
}

func TestAppendAssistantMessage(t *testing.T) {
	payload := map[string]any{
		"model":    "test",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	msg := map[string]any{"role": "assistant", "content": "hello"}

	appendAssistantMessage(payload, msg)

	msgs, ok := payload["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	got, ok := msgs[1].(map[string]any)
	if !ok || got["role"] != "assistant" || got["content"] != "hello" {
		t.Fatalf("expected appended assistant message, got %v", msgs[1])
	}
}

func TestNewStreamContinuation_Disabled(t *testing.T) {
	p := &Proxy{}
	cfg := config.Config{}
	cfg.Governance.StreamContinuation = &config.StreamContinuationConfig{
		Enabled: config.PtrTo(false),
	}
	gw := &gateway.NenyaGateway{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	opts := streamResponseOpts{
		gw:           gw,
		target:       routing.UpstreamTarget{Provider: "p", Model: "m"},
		sourceFormat: "openai",
	}
	cont := p.newStreamContinuation(gw, opts)
	if cont != nil {
		t.Fatal("expected nil when disabled")
	}
}

func TestNewStreamContinuation_AnthropicTarget(t *testing.T) {
	p := &Proxy{}
	cfg := config.Config{}
	cfg.Governance.StreamContinuation = &config.StreamContinuationConfig{
		Enabled: config.PtrTo(true),
	}
	gw := &gateway.NenyaGateway{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	opts := streamResponseOpts{
		gw:           gw,
		target:       routing.UpstreamTarget{Provider: "provider", Model: "m", Format: "anthropic"},
		sourceFormat: "openai",
	}
	cont := p.newStreamContinuation(gw, opts)
	if cont != nil {
		t.Fatal("expected nil for Anthropic format")
	}

	opts2 := streamResponseOpts{
		gw:           gw,
		target:       routing.UpstreamTarget{Provider: "provider", Model: "m"},
		sourceFormat: "anthropic",
	}
	cont = p.newStreamContinuation(gw, opts2)
	if cont != nil {
		t.Fatal("expected nil for Anthropic source format")
	}
}

func TestNewStreamContinuation_RespectsMaxAttempts(t *testing.T) {
	p := &Proxy{}
	cfg := config.Config{}
	cfg.Governance.StreamContinuation = &config.StreamContinuationConfig{
		Enabled:     config.PtrTo(true),
		MaxAttempts: 1,
	}
	gw := &gateway.NenyaGateway{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	opts := streamResponseOpts{
		gw:           gw,
		target:       routing.UpstreamTarget{Provider: "provider", Model: "m"},
		sourceFormat: "openai",
	}
	if cont := p.newStreamContinuation(gw, opts); cont != nil {
		t.Fatal("expected nil when max_attempts < 2")
	}

	cfg.Governance.StreamContinuation.MaxAttempts = 3
	cont := p.newStreamContinuation(gw, opts)
	if cont == nil {
		t.Fatal("expected continuation when max_attempts >= 2")
	}
	if cont.maxAttempts != 3 {
		t.Fatalf("expected maxAttempts=3, got %d", cont.maxAttempts)
	}
}
func newContinuationProxy(t *testing.T, upstreamURL string) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(60)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(100000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Governance.StreamContinuation = &config.StreamContinuationConfig{
		Enabled:       config.PtrTo(true),
		MaxAttempts:   2,
		SameModelOnly: config.PtrTo(true),
	}
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider": {
			URL:       upstreamURL + "/v1/chat/completions",
			AuthStyle: "none",
		},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models: []config.AgentModel{
				{Provider: "test-provider", Model: "test-model"},
			},
		},
	}
	secrets := &config.SecretsConfig{
		ClientToken: "test-token",
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

func TestNewStreamContinuation_SameModelOnly(t *testing.T) {
	p := &Proxy{}
	newGw := func() *gateway.NenyaGateway {
		return &gateway.NenyaGateway{
			Config: config.Config{},
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
	}
	opts := streamResponseOpts{
		gw:           nil,
		target:       routing.UpstreamTarget{Provider: "p", Model: "m"},
		sourceFormat: "openai",
	}

	// Accessor defaults: absent section -> same_model_only=true.
	gw := newGw()
	opts.gw = gw
	if !gw.Config.Governance.StreamContinuationSameModelOnly() {
		t.Fatal("expected same_model_only=true when section absent")
	}

	// newStreamContinuation requires an enabled section to build state.
	gw.Config.Governance.StreamContinuation = &config.StreamContinuationConfig{
		Enabled:       config.PtrTo(true),
		SameModelOnly: config.PtrTo(true),
	}
	if cont := p.newStreamContinuation(gw, opts); cont == nil {
		t.Fatal("expected continuation when same_model_only=true")
	}

	// Accessor: explicit false is honored even when the section is present.
	gw.Config.Governance.StreamContinuation.SameModelOnly = config.PtrTo(false)
	if gw.Config.Governance.StreamContinuationSameModelOnly() {
		t.Fatal("expected same_model_only=false when explicitly set")
	}
	if cont := p.newStreamContinuation(gw, opts); cont != nil {
		t.Fatal("expected nil continuation when same_model_only=false (cross-model resume unsupported)")
	}

	// Accessor: nil field (defaults not yet applied) still reads as true.
	gw.Config.Governance.StreamContinuation.SameModelOnly = nil
	if !gw.Config.Governance.StreamContinuationSameModelOnly() {
		t.Fatal("expected same_model_only=true when field unset")
	}
}

// TestStreamResponse_ContinuationResumesCutStream verifies that when an
// upstream SSE stream is cut mid-generation (content seen, no finish_reason, no
// [DONE]), the proxy appends the partial assistant message, re-dispatches the
// same target, and the client receives the full content across both attempts.
func TestStreamResponse_ContinuationResumesCutStream(t *testing.T) {
	var calls atomic.Int32
	var secondBody atomic.Pointer[[]byte]
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n")
			return
		}
		body, _ := io.ReadAll(r.Body)
		secondBody.Store(&body)
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	p := newContinuationProxy(t, upstream.URL)
	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	respBody := rec.Body.String()
	if !strings.Contains(respBody, "hello ") {
		t.Errorf("expected partial content 'hello ', got: %s", respBody)
	}
	if !strings.Contains(respBody, "world") {
		t.Errorf("expected continued content 'world', got: %s", respBody)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 upstream calls (original + continuation), got %d", got)
	}

	// The continuation request must carry the partial assistant message so the
	// upstream can resume from where the stream was cut.
	contReq := secondBody.Load()
	if contReq == nil {
		t.Fatal("expected continuation request body")
	}
	var contPayload map[string]any
	if err := json.Unmarshal(*contReq, &contPayload); err != nil {
		t.Fatalf("failed to parse continuation request body: %v", err)
	}
	msgs, ok := contPayload["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("expected 2 messages in continuation payload, got %d", len(msgs))
	}
	assistant, ok := msgs[1].(map[string]any)
	if !ok || assistant["role"] != "assistant" {
		t.Fatalf("expected assistant message appended, got %v", msgs[1])
	}
	if assistant["content"] != "hello " {
		t.Fatalf("expected appended content 'hello ', got %q", assistant["content"])
	}

	var buf bytes.Buffer
	gw := p.Gateway()
	gw.Metrics.WritePrometheus(&buf)
	if !strings.Contains(buf.String(), `nenya_stream_continuations_total{model="test-model", provider="test-provider", reason="recovered"} 1`) {
		t.Errorf("expected recovered metric, got: %s", buf.String())
	}
}

// TestStreamResponse_NoContinuationWhenCutOnExhaustedAttempt verifies that when
// the max attempts are exhausted the proxy gives up and indicates it.
func TestStreamResponse_ContinuationExhaustedGivesUp(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial \"}}]}\n\n")
		if n == 1 {
			w.(http.Flusher).Flush()
			return
		}
		// Second attempt also cuts, which is the final attempt with max_attempts=2.
		// The client sees gateway_error + [DONE] because suppressCutError=false.
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	p := newContinuationProxy(t, upstream.URL)
	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	respBody := rec.Body.String()
	if !strings.Contains(respBody, "partial ") {
		t.Errorf("expected partial content, got: %s", respBody)
	}
	// Expect exactly 2 upstream calls because max_attempts=2 means one continuation retry.
	// Both attempts cut, so the second attempt writes the final gateway_error terminator.
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", got)
	}
	if !strings.Contains(respBody, "gateway_error") {
		t.Errorf("expected final gateway_error terminator, got: %s", respBody)
	}
	var buf bytes.Buffer
	gw := p.Gateway()
	gw.Metrics.WritePrometheus(&buf)
	if !strings.Contains(buf.String(), `reason="gave_up_exhausted"`) {
		t.Errorf("expected gave_up_exhausted metric, got: %s", buf.String())
	}
}
