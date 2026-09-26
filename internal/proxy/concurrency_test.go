package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/testutil"
)

// newConcurrencyChatProxy builds a Proxy whose test-provider has a
// per-model concurrency cap of 1 for "test-model". The upstream URL is
// expected to be a streaming SSE chat-completions endpoint.
func newConcurrencyChatProxy(t *testing.T, upstreamURL string) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(60)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(100000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider": {
			URL:                   upstreamURL + "/v1/chat/completions",
			AuthStyle:             "none",
			MaxConcurrentRequests: 1,
			ModelConcurrency:      map[string]int{"test-model": 1},
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
	secrets := &config.SecretsConfig{ClientToken: "test-token"}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

// TestHandleChatCompletions_ConcurrencySerializationAtLimit drives two
// concurrent streaming requests to a provider with model_concurrency=1 and a
// slow upstream. The gate must serialize them: the second upstream call may
// only begin after the first request's SSE stream has finished, so the
// upstream's concurrently-active handler count never exceeds 1 and the
// limiter drains to zero afterwards.
func TestHandleChatCompletions_ConcurrencySerializationAtLimit(t *testing.T) {
	var (
		mu        sync.Mutex
		active    int
		maxActive int
	)
	const hold = 250 * time.Millisecond

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			active--
			mu.Unlock()
		}()

		time.Sleep(hold) // widen the overlap window if serialization is broken
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	p := newConcurrencyChatProxy(t, upstream.URL)
	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`

	var wg sync.WaitGroup
	statuses := make([]int, 2)
	recorders := make([]*httptest.ResponseRecorder, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			recorders[i] = rec
			statuses[i] = rec.Code
		}(i)
	}
	wg.Wait()

	mu.Lock()
	got := maxActive
	mu.Unlock()

	if got != 1 {
		t.Errorf("max concurrent upstream calls = %d, want 1 (serialization at model_concurrency=1)", got)
	}
	for i, code := range statuses {
		if code != http.StatusOK {
			t.Errorf("request %d status = %d, want 200", i, code)
		}
	}
	for i, rec := range recorders {
		recBody, _ := io.ReadAll(rec.Body)
		if !strings.Contains(string(recBody), "hello") {
			t.Errorf("request %d body missing hello: %s", i, string(recBody))
		}
	}

	// Every held slot must be released after the streams complete.
	inflight := p.Gateway().ConcurrencyLimiter.Inflight("test-provider/test-model")
	if inflight != 0 {
		t.Errorf("inflight after completion = %d, want 0 (slot leak)", inflight)
	}
}

// TestHandleUpstreamError_ZAI1302_NoCooldownNoCB verifies that a ZAI
// concurrency-limit error (1302) is retried with a short fixed delay and
// does NOT take the rate-limit path (which would activate cooldown and count
// circuit-breaker failures as saturation-is-illness).
func TestHandleUpstreamError_ZAI1302_NoCooldownNoCB(t *testing.T) {
	cfg := &config.Config{}
	gw := newTestGateway(cfg, map[string]*config.Provider{"zai": {Name: "zai"}})
	p := &Proxy{}

	target := routing.UpstreamTarget{Provider: "zai", Model: "glm-5.3", CoolKey: "agent|x|zai|glm-5.3"}
	targets := []routing.UpstreamTarget{target}
	action := upstreamAction{
		resp: &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"1302"}}`)),
		},
	}
	action.body, _ = io.ReadAll(action.resp.Body)

	signal, delay := p.handleUpstreamError(gw, 0, targets, target, 5*time.Second, "agent", action)

	if signal != retrySignalRetry {
		t.Fatal("1302 must be retried")
	}
	if delay < concurrencyRetryBase || delay > concurrencyRetryBase+concurrencyRetryJitter {
		t.Fatalf("1302 delay = %v, want ~%v", delay, concurrencyRetryBase)
	}
	// The breaker must not have been opened by saturation.
	if allow := gw.AgentState.CB.Allow(target.CoolKey); !allow {
		t.Fatal("1302 must not punish the circuit breaker")
	}
}

// TestHandleUpstreamError_ZAI1303_KeepsRateLimitPath is the control: the
// frequency-limit code 1303 stays on the rate-limit path (delay 0 meaning no
// fixed concurrency wait — regular backoff/cooldown semantics), unlike 1302.
func TestHandleUpstreamError_ZAI1303_KeepsRateLimitPath(t *testing.T) {
	cfg := &config.Config{}
	gw := newTestGateway(cfg, map[string]*config.Provider{"zai": {Name: "zai"}})
	p := &Proxy{}

	target := routing.UpstreamTarget{Provider: "zai", Model: "glm-5.3", CoolKey: "agent|x|zai|glm-5.3"}
	targets := []routing.UpstreamTarget{target}
	action := upstreamAction{
		resp: &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"1303"}}`)),
		},
	}
	action.body, _ = io.ReadAll(action.resp.Body)

	signal, delay := p.handleUpstreamError(gw, 0, targets, target, 5*time.Second, "agent", action)

	if signal != retrySignalRetry {
		t.Fatal("1303 must be retried")
	}
	if delay != 0 {
		t.Fatalf("1303 delay = %v, want 0 (rate-limit path, no fixed concurrency wait)", delay)
	}
}

// TestHandleChatCompletions_OversizedSessionDispatched reproduces the
// NENYA-70 incident: a session whose token estimate exceeds the provider's
// entire TPM capacity used to be skipped on every target ("all upstream
// targets exhausted", attempts: 0) because the TPM bucket can never refill
// past its limit. The oversize request must be dispatched instead.
func TestHandleChatCompletions_OversizedSessionDispatched(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(60)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(100) // tiny on purpose: forces the oversize path
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider": {URL: upstream.URL + "/v1/chat/completions", AuthStyle: "none"},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models:   []config.AgentModel{{Provider: "test-provider", Model: "test-model"}},
		},
	}
	secrets := &config.SecretsConfig{ClientToken: "test-token"}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)

	big := strings.Repeat("lorem ipsum dolor sit amet consectetur ", 300) // ~2.5k tokens ≫ 100 TPM
	body := fmt.Sprintf(`{"model":"test-agent","messages":[{"role":"user","content":%q}]}`, big)
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (oversized session must be dispatched, not exhausted); body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("expected streamed content, got: %s", rec.Body.String())
	}
}

// TestConcurrencyRetryDelayRange sanity-checks the fixed delay window.
func TestConcurrencyRetryDelayRange(t *testing.T) {
	for range 50 {
		d := concurrencyRetryDelay()
		if d < concurrencyRetryBase || d > concurrencyRetryBase+concurrencyRetryJitter {
			t.Fatalf("delay %v out of range", d)
		}
	}
}
