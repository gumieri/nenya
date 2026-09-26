package proxy

import (
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
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/testutil"
)

// newRetryTestProxy builds a proxy whose agent lists the same provider/model
// chainSize times (the documented failover-within-provider idiom) so a single
// entry exercises the same-target retry path.
func newRetryTestProxy(t *testing.T, upstreamURL string, chainSize int, tune func(cfg *config.Config)) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(1000)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(1000000)
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider": {URL: upstreamURL + "/v1/chat/completions", AuthStyle: "none"},
	}
	models := make([]config.AgentModel, 0, chainSize)
	for range chainSize {
		models = append(models, config.AgentModel{Provider: "test-provider", Model: "test-model"})
	}
	cfg.Agents = map[string]config.AgentConfig{
		"retry-agent": {Strategy: "fallback", Models: models},
	}
	if tune != nil {
		tune(cfg)
	}
	secrets := &config.SecretsConfig{ClientToken: "test-token", ProviderKeys: map[string]string{}}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = gw.Shutdown(ctx)
	})
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

func writeChatSuccess(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"id":    "chat-good",
		"model": "test-model",
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"message":       map[string]interface{}{"role": "assistant", "content": "hello world"},
				"finish_reason": "stop",
			},
		},
	})
}

const retryTestBody = `{"model":"retry-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`

// A retryable failure on a single-target agent must re-attempt the same target.
func TestRun_SingleTargetRetryable5xxRetried(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		writeChatSuccess(w)
	}))
	defer up.Close()

	rec := postChatJSON(t, newRetryTestProxy(t, up.URL, 1, nil), retryTestBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after same-target retry, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 upstream hits (1 retry), got %d", got)
	}
}

// An opaque 4xx body (JSON object, no error envelope) is retryable and must be
// re-attempted on a single-target agent.
func TestRun_SingleTargetOpaque4xxRetried(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"model":"test-model"}`))
			return
		}
		writeChatSuccess(w)
	}))
	defer up.Close()

	rec := postChatJSON(t, newRetryTestProxy(t, up.URL, 1, nil), retryTestBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after opaque-4xx retry, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 upstream hits (1 retry), got %d", got)
	}
}

// A structured client error (error envelope) stays non-retryable: it surfaces
// immediately with the upstream status, no second dispatch.
func TestRun_SingleTargetEnveloped4xxNotRetried(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request shape"}}`))
	}))
	defer up.Close()

	rec := postChatJSON(t, newRetryTestProxy(t, up.URL, 1, nil), retryTestBody)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected upstream 400 passthrough, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected a single upstream hit, got %d", got)
	}
}

// governance.retry_opaque_4xx = false opts out of the opaque-body rule.
func TestRun_Opaque4xxOptOut(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"model":"test-model"}`))
	}))
	defer up.Close()

	rec := postChatJSON(t, newRetryTestProxy(t, up.URL, 1, func(cfg *config.Config) {
		cfg.Governance.RetryOpaque4xx = config.PtrTo(false)
	}), retryTestBody)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected upstream 400 passthrough when opted out, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected a single upstream hit when opted out, got %d", got)
	}
}

// A persistently retryable failure on a single target is bounded by the
// governance max_retry_attempts budget (1 initial + N same-target retries).
func TestRun_SingleTargetExhaustionBounded(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer up.Close()

	rec := postChatJSON(t, newRetryTestProxy(t, up.URL, 1, func(cfg *config.Config) {
		cfg.Governance.MaxRetryAttempts = 2
	}), retryTestBody)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 after exhaustion, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("expected 3 upstream hits (1 initial + 2 retries), got %d", got)
	}
}

// A retryable failure with a later target still advances (forward sweep), and
// must not be retried on the first target.
func TestRun_MultiTargetRetryableAdvances(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		writeChatSuccess(w)
	}))
	defer up.Close()

	rec := postChatJSON(t, newRetryTestProxy(t, up.URL, 2, nil), retryTestBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after failover, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 upstream hits (failover), got %d", got)
	}
}

// The MCP buffered sweep must surface the same retryable signal so a
// single-target 5xx failure is re-attempted there too.
func TestHandleBufferedAction_RetryableSignal(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	p := newRetryTestProxy(t, up.URL, 1, nil)
	gw := p.Gateway()

	target := routing.UpstreamTarget{Provider: "test-provider", Model: "test-model", CoolKey: "retry-agent:test-provider:test-model"}
	targets := []routing.UpstreamTarget{target}
	action := upstreamAction{
		kind: actionError,
		resp: &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"boom"}}`)),
		},
		cancel: func() {},
	}
	// A 20ms deadline short-circuits the internal backoff while keeping the
	// retry decision observable.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	result, outcome := p.handleBufferedAction(ctx, gw, 0, targets, target, 0, "retry-agent", action, 1, 0, 0, pipeline.CanarResult{})
	if result != nil {
		t.Fatalf("expected no buffered result on error, got %v", result)
	}
	if outcome.step != bufferedStepRetry {
		t.Fatalf("expected bufferedStepRetry for a retryable 5xx, got %v", outcome.step)
	}
}

// The opaque-4xx rule defaults on through applyGovernanceDefaults and the
// nil-pointer accessor, so a config that never mentions the field still
// retries an opaque body.
func TestRun_Opaque4xxDefaultEnabled(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"model":"test-model"}`))
			return
		}
		writeChatSuccess(w)
	}))
	defer up.Close()

	// No tune callback: RetryOpaque4xx is left nil (unset).
	p := newRetryTestProxy(t, up.URL, 1, nil)
	if !p.Gateway().Config.Governance.Opaque4xxRetryEnabled() {
		t.Fatal("expected retry_opaque_4xx to default to enabled")
	}

	rec := postChatJSON(t, p, retryTestBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after default opaque-4xx retry, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 upstream hits by default, got %d", got)
	}
}

// A retryable 4xx whose retry budget is exhausted is relayed with the real
// upstream status, not masked as a generic 503.
func TestRun_Retryable4xxRelayedAfterExhaustion(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"model":"test-model"}`))
	}))
	defer up.Close()

	rec := postChatJSON(t, newRetryTestProxy(t, up.URL, 1, func(cfg *config.Config) {
		cfg.Governance.MaxRetryAttempts = 1
	}), retryTestBody)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected the upstream 422 to be relayed after exhaustion, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 upstream hits (1 initial + 1 retry), got %d", got)
	}
}

// max_retries bounds retries (N retries), uniform for error and same-target
// repeats: max_retries=2 allows the initial attempt plus two retries.
func TestRun_MaxRetriesBoundsRetries(t *testing.T) {
	cases := []struct {
		name         string
		maxRetries   int
		wantAttempts int32
	}{
		{"zero uses governance default", 0, 4},
		{"one", 1, 2},
		{"two", 2, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			}))
			defer up.Close()

			p := newRetryTestProxy(t, up.URL, 1, func(cfg *config.Config) {
				cfg.Agents["retry-agent"] = config.AgentConfig{
					Strategy:   "fallback",
					MaxRetries: tc.maxRetries,
					Models:     []config.AgentModel{{Provider: "test-provider", Model: "test-model"}},
				}
			})
			rec := postChatJSON(t, p, retryTestBody)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503 after exhaustion, got %d", rec.Code)
			}
			if got := calls.Load(); got != tc.wantAttempts {
				t.Fatalf("max_retries=%d: expected %d attempts, got %d", tc.maxRetries, tc.wantAttempts, got)
			}
		})
	}
}

// A retryable 4xx must not be replayed after a later target produced a
// different (5xx) failure: the exhaustion relay reflects the last failure and
// the request exhausts with a generic 503.
func TestRun_Stale4xxSnapshotCleared(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"model":"test-model"}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
		}
	}))
	defer up.Close()

	p := newRetryTestProxy(t, up.URL, 2, func(cfg *config.Config) {
		cfg.Governance.MaxRetryAttempts = 1
	})
	rec := postChatJSON(t, p, retryTestBody)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 exhaustion (last failure was a 5xx), got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

// A stale 4xx snapshot must not survive a later network failure.
func TestRun_Stale4xxSnapshotClearedByNetworkError(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"model":"test-model"}`))
		default:
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
	}))
	defer up.Close()

	p := newRetryTestProxy(t, up.URL, 2, func(cfg *config.Config) {
		cfg.Governance.MaxRetryAttempts = 1
	})
	rec := postChatJSON(t, p, retryTestBody)
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("stale 4xx status leaked after a later network failure: %d", rec.Code)
	}
}

// The same-target retry also covers local/transport failures: a single-target
// agent whose upstream closes the connection then succeeds must be retried
// rather than exhausting.
func TestRun_SingleTargetNetworkErrorRetried(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		writeChatSuccess(w)
	}))
	defer up.Close()

	rec := postChatJSON(t, newRetryTestProxy(t, up.URL, 1, func(cfg *config.Config) {
		cfg.Governance.MaxRetryAttempts = 1
	}), retryTestBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after network-error same-target retry, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 upstream hits (1 retry), got %d", got)
	}
}

// A same-target repeat must start a fresh upstream request: a stream 5xx then a
// clean SSE stream on the retry yields a normal streaming response.
func TestRun_SingleTargetStreamRetried(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	body := `{"model":"retry-agent","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := postChatJSON(t, newRetryTestProxy(t, up.URL, 1, func(cfg *config.Config) {
		cfg.Governance.MaxRetryAttempts = 1
	}), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after streaming retry, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 upstream hits (1 retry), got %d", got)
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("expected retried stream content, got %q", rec.Body.String())
	}
}

func TestOpaque4xxBody(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"bare model object", http.StatusBadRequest, `{"model":"deepseek-v4.1-flash"}`, true},
		{"unknown fields only", http.StatusBadRequest, `{"foo":"bar"}`, true},
		{"empty object", http.StatusBadRequest, `{}`, false},
		{"error envelope", http.StatusBadRequest, `{"error":{"message":"bad"}}`, false},
		{"detail envelope", http.StatusBadRequest, `{"detail":"invalid"}`, false},
		{"message envelope", http.StatusBadRequest, `{"message":"bad"}`, false},
		{"type envelope", http.StatusBadRequest, `{"type":"error"}`, false},
		{"json array", http.StatusBadRequest, `[1,2,3]`, false},
		{"non-json", http.StatusBadRequest, `not json`, false},
		{"empty body", http.StatusBadRequest, ``, false},
		{"413 excluded", http.StatusRequestEntityTooLarge, `{"model":"x"}`, false},
		{"422 opaque", http.StatusUnprocessableEntity, `{"model":"x"}`, true},
		{"401 not considered", http.StatusUnauthorized, `{"model":"x"}`, false},
		{"404 not considered", http.StatusNotFound, `{"model":"x"}`, false},
		{"429 not considered", http.StatusTooManyRequests, `{"model":"x"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := opaque4xxBody(tc.status, []byte(tc.body)); got != tc.want {
				t.Errorf("opaque4xxBody(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}
