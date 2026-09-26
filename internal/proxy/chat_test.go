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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/testutil"
)

func newChatProxy(t *testing.T, upstreamURL string) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(60)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(100000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider": {
			URL:       upstreamURL + "/v1/chat/completions",
			AuthStyle: "none",
		},
		"deepseek": {
			URL:       upstreamURL + "/v1/chat/completions",
			AuthStyle: "bearer",
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
		ProviderKeys: map[string]string{
			"deepseek": "test-api-key",
		},
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

func TestHandleChatCompletions_ValidUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	respBody, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(respBody), "hello") {
		t.Errorf("expected response to contain 'hello', got: %s", string(respBody))
	}
}

func TestHandleChatCompletions_MissingModel(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusBadRequest)
}

func TestHandleChatCompletions_EmptyModel(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := `{"model":"","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusBadRequest)
}

func TestHandleChatCompletions_ModelTooLong(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	longModel := strings.Repeat("a", MaxModelNameLength+1)
	payload := fmt.Sprintf(`{"model":"%s","messages":[{"role":"user","content":"hi"}]}`, longModel)
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", payload)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusBadRequest)
}

func TestHandleChatCompletions_NonStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chat-123",
			"object":  "chat.completion",
			"created": 1234567890,
			"model":   "test-model",
			"choices": []interface{}{
				map[string]interface{}{
					"index": 0,
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "hello world",
					},
				},
			},
		})
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	body := `{"model":"test-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	respBody, _ := io.ReadAll(rec.Body)
	if strings.Contains(string(respBody), "data:") {
		t.Errorf("expected non-streaming response, got SSE: %s", string(respBody))
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		t.Fatalf("expected JSON response, got error: %v, body: %s", err, string(respBody))
	}
	if resp["id"] != "chat-123" {
		t.Errorf("expected id=chat-123, got %v", resp["id"])
	}
}

func TestHandleChatCompletions_NonStreamingEmptyResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(nil)
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	body := `{"model":"test-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Logf("got %d for empty non-streaming — acceptable without upstream error", rec.Code)
	}
}

// TestHandleChatCompletions_ProviderRetryablePhrases pins NENYA-26: a
// provider-configured retryable_phrases entry makes an otherwise
// non-retryable 4xx body fail over to the next target. The phrase-matched
// log line is the observable delta over the generic multi-target fallthrough
// (which also retries non-retryable errors without recording a phrase hit),
// so the test captures slog records and asserts the distinct warning fired.
func TestHandleChatCompletions_ProviderRetryablePhrases(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"quota bridge hiccup: my_custom_transient_failure"}}`))
			return
		}
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
	}))
	defer upstream.Close()

	var mu sync.Mutex
	phraseHits := 0
	// Wrap the default logger to count phrase-match warnings.
	capturing := slog.New(newPhraseCaptureHandler(&mu, &phraseHits))

	cfg := config.Config{
		Server: config.ServerConfig{MaxBodyBytes: 10 << 20},
		Governance: config.GovernanceConfig{
			RatelimitMaxRPM: config.PtrTo(60),
			RatelimitMaxTPM: config.PtrTo(100000),
		},
		Bouncer: config.BouncerConfig{Enabled: config.PtrTo(false)},
		Providers: map[string]config.ProviderConfig{
			"test-provider": {
				URL:              upstream.URL + "/v1/chat/completions",
				AuthStyle:        "none",
				RetryablePhrases: []string{"my_custom_transient_failure"},
			},
		},
		Agents: map[string]config.AgentConfig{
			"phrase-agent": {
				Models: []config.AgentModel{
					{Provider: "test-provider", Model: "test-model"},
					{Provider: "test-provider", Model: "test-model"},
				},
			},
		},
	}
	secrets := &config.SecretsConfig{ClientToken: "test-token", ProviderKeys: map[string]string{}}
	gw := gateway.New(context.Background(), cfg, secrets, capturing)
	p := &Proxy{}
	p.StoreGateway(gw)

	body := `{"model":"phrase-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if attempts.Load() != 2 {
		t.Fatalf("expected configured phrase to trigger failover (2 upstream hits), got %d attempts", attempts.Load())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after phrase-triggered failover, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if phraseHits == 0 {
		t.Fatal("expected the provider-configured retryable phrase path to fire (no phrase-match log captured)")
	}
}

// phraseCaptureHandler wraps slog.Handler and counts records carrying the
// NENYA-26 phrase-match message.
type phraseCaptureHandler struct {
	inner slog.Handler
	mu    *sync.Mutex
	hits  *int
}

func newPhraseCaptureHandler(mu *sync.Mutex, hits *int) slog.Handler {
	return &phraseCaptureHandler{inner: slog.NewTextHandler(io.Discard, nil), mu: mu, hits: hits}
}

func (h *phraseCaptureHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level <= slog.LevelWarn
}

func (h *phraseCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, "provider-configured retryable phrase matched") {
		h.mu.Lock()
		*h.hits++
		h.mu.Unlock()
	}
	return nil
}

func (h *phraseCaptureHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *phraseCaptureHandler) WithGroup(name string) slog.Handler       { return h }

// newMultiTargetChatProxy builds a chat proxy whose agent routes to the same
// upstream twice, for failover tests.
func newMultiTargetChatProxy(t *testing.T, upstreamURL, agentName string) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider": {URL: upstreamURL + "/v1/chat/completions", AuthStyle: "none"},
	}
	cfg.Agents = map[string]config.AgentConfig{
		agentName: {
			Strategy: "fallback",
			Models: []config.AgentModel{
				{Provider: "test-provider", Model: "test-model"},
				{Provider: "test-provider", Model: "test-model"},
			},
		},
	}
	secrets := &config.SecretsConfig{ClientToken: "test-token", ProviderKeys: map[string]string{}}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

// TestHandleChatCompletions_EmbeddedErrorFailsOver pins NENYA-28: an
// HTTP-200 body carrying a provider error object (Ollama-style string) is a
// failed upstream attempt — the retry loop fails over instead of relaying
// the error as a completion.
func TestHandleChatCompletions_EmbeddedErrorFailsOver(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if attempts.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": "model 'llama3:8b' is not loaded, try pulling it first",
			})
			return
		}
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
	}))
	defer upstream.Close()

	p := newMultiTargetChatProxy(t, upstream.URL, "embedded-err-agent")

	body := `{"model":"embedded-err-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if attempts.Load() != 2 {
		t.Fatalf("expected embedded-error body to trigger failover (2 upstream hits), got %d attempts", attempts.Load())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after failover, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected JSON response: %v", err)
	}
	if resp["id"] != "chat-good" {
		t.Errorf("expected the healthy completion (chat-good), got %v", resp["id"])
	}
}

// TestHandleChatCompletions_NullErrorFieldRelayed guards NENYA-28 against
// false positives: some providers emit "error": null on success — that body
// must be relayed as a normal completion.
func TestHandleChatCompletions_NullErrorFieldRelayed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":    "chat-ok",
			"model": "test-model",
			"error": nil,
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"message":       map[string]interface{}{"role": "assistant", "content": "fine"},
					"finish_reason": "stop",
				},
			},
		})
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	body := `{"model":"test-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected null-error body to be relayed as success, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected JSON completion body: %v", err)
	}
	if resp["id"] != "chat-ok" {
		t.Errorf("expected the original completion (chat-ok) relayed, got %v", resp["id"])
	}
}

// TestHandleChatCompletions_RequestScopedErrorNoRotation pins NENYA-42: a
// provider-configured request_scoped_errors rule marks the error as the
// client's fault — the request fails fast (1 upstream hit) with the upstream
// status surfaced, instead of sweeping the remaining targets. Without the
// rule, the same error sweeps the fallback chain (2 hits).
func TestHandleChatCompletions_RequestScopedErrorNoRotation(t *testing.T) {
	t.Run("with rule: fails fast without rotation", func(t *testing.T) {
		var hits atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid parameter: temperature must be a number"}}`))
		}))
		defer upstream.Close()

		p := newMultiTargetChatProxy(t, upstream.URL, "scoped-agent")
		// Add the request-scoped rule post-construction (same pattern as
		// embeddings_retry_test.go's provider mutations).
		p.Gateway().Providers["test-provider"].RequestScopedErrors = []config.RequestScopedErrorRule{
			{Status: 401, MessagePattern: "invalid parameter"},
		}

		body := `{"model":"scoped-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)

		if hits.Load() != 1 {
			t.Fatalf("expected request-scoped error to fail fast (1 upstream hit), got %d", hits.Load())
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected upstream 401 surfaced to client, got %d (body: %s)", rec.Code, rec.Body.String())
		}
		var resp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("expected JSON error body: %v", err)
		}
		if !strings.Contains(resp.Error.Message, "invalid parameter") {
			t.Errorf("expected upstream message surfaced, got %q", resp.Error.Message)
		}
	})

	t.Run("without rule: sweeps the fallback chain", func(t *testing.T) {
		var hits atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid parameter: temperature must be a number"}}`))
		}))
		defer upstream.Close()

		p := newMultiTargetChatProxy(t, upstream.URL, "sweep-agent")
		body := `{"model":"sweep-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)

		// Without the rule the generic non-retryable fallthrough sweeps both
		// targets. The last 401 that consumed no retry budget is relayed with
		// its real status (a truthful client-class error), not masked as 503.
		if hits.Load() != 2 {
			t.Fatalf("expected fallback sweep to hit upstream twice, got %d", hits.Load())
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected the swept 401 relayed to the client, got %d (body: %s)", rec.Code, rec.Body.String())
		}
	})
}

// TestHandleChatCompletions_NonStreamingNetworkErrorFinishReasonFailsOver pins NENYA-12:
// a 200 completion whose terminal finish_reason is a network-failure variant
// is a failed upstream attempt — the retry loop must fail over to the next
// target instead of relaying the broken completion.
func TestHandleChatCompletions_NonStreamingNetworkErrorFinishReasonFailsOver(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if attempts.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":    "chat-broken",
				"model": "test-model",
				"choices": []interface{}{
					map[string]interface{}{
						"index":         0,
						"message":       map[string]interface{}{"role": "assistant", "content": "partial"},
						"finish_reason": "network_error",
					},
				},
			})
			return
		}
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
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{MaxBodyBytes: 10 << 20},
		Governance: config.GovernanceConfig{
			RatelimitMaxRPM: config.PtrTo(60),
			RatelimitMaxTPM: config.PtrTo(100000),
		},
		Bouncer: config.BouncerConfig{Enabled: config.PtrTo(false)},
		Providers: map[string]config.ProviderConfig{
			"test-provider": {URL: upstream.URL + "/v1/chat/completions", AuthStyle: "none"},
		},
		Agents: map[string]config.AgentConfig{
			"failover-agent": {
				Models: []config.AgentModel{
					{Provider: "test-provider", Model: "test-model"},
					{Provider: "test-provider", Model: "test-model"},
				},
			},
		},
	}
	secrets := &config.SecretsConfig{ClientToken: "test-token", ProviderKeys: map[string]string{}}
	gw := gateway.New(context.Background(), cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)

	body := `{"model":"failover-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if attempts.Load() != 2 {
		t.Fatalf("expected failover to hit upstream twice, got %d attempts", attempts.Load())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after failover, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected JSON response: %v", err)
	}
	if resp["id"] != "chat-good" {
		t.Errorf("expected the healthy completion (chat-good), got %v", resp["id"])
	}
}

// TestHandleChatCompletions_NonStreamingNetworkErrorFinishReasonExhausted
// verifies the exhaustion path: when every target returns a network-error
// terminal, the client receives a typed upstream error instead of the broken
// completion body.
func TestHandleChatCompletions_NonStreamingNetworkErrorFinishReasonExhausted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":    "chat-broken",
			"model": "test-model",
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"message":       map[string]interface{}{"role": "assistant", "content": "partial"},
					"finish_reason": "network-error",
				},
			},
		})
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	body := `{"model":"test-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("expected typed error after exhaustion, got 200 relaying the broken completion: %s", rec.Body.String())
	}
	var resp struct {
		Error struct {
			ErrorKind string `json:"error_kind"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected JSON error body: %v", err)
	}
	if resp.Error.ErrorKind == "" {
		t.Errorf("expected structured error_kind in exhaustion response, got body: %s", rec.Body.String())
	}
}

func TestHandleChatCompletions_InvalidJSON(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := `{invalid json}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusBadRequest)
}

func TestHandleChatCompletions_UnknownModel(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := `{"model":"unknown-model-xyz","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusBadRequest)
}

func TestHandleChatCompletions_AgentWithModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"agent response\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			MaxBodyBytes: 10 << 20,
		},
		Governance: config.GovernanceConfig{
			RatelimitMaxRPM: config.PtrTo(60),
			RatelimitMaxTPM: config.PtrTo(100000),
		},
		Bouncer: config.BouncerConfig{
			Enabled: config.PtrTo(false),
		},
		Providers: map[string]config.ProviderConfig{
			"test-provider": {
				URL:       upstream.URL + "/v1/chat/completions",
				AuthStyle: "none",
			},
		},
		Agents: map[string]config.AgentConfig{
			"my-agent": {
				Models: []config.AgentModel{
					{Provider: "test-provider", Model: "test-model"},
				},
			},
		},
	}
	secrets := &config.SecretsConfig{
		ClientToken:  "test-token",
		ProviderKeys: map[string]string{},
	}
	gw := gateway.New(context.Background(), cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)

	body := strings.NewReader(`{"model":"my-agent","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	respBody, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(respBody), "agent response") {
		t.Errorf("expected response to contain 'agent response', got: %s", string(respBody))
	}
}

func TestHandleChatCompletions_AgentNoModels(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	p.Gateway().Config.Agents = map[string]config.AgentConfig{
		"empty-agent": {
			Models: []config.AgentModel{},
		},
	}

	body := strings.NewReader(`{"model":"empty-agent","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

func TestHandleEmbeddings_ValidUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data":   []map[string]interface{}{{"embedding": []float64{0.1, 0.2}}},
		}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	body := strings.NewReader(`{"model":"deepseek-v4-flash","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected JSON response, got error: %v", err)
	}
	if resp["object"] != "list" {
		t.Errorf("expected object=list, got %v", resp["object"])
	}
}

func TestHandleEmbeddings_MissingModel(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := strings.NewReader(`{"input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleEmbeddings_UnknownModel(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := strings.NewReader(`{"model":"unknown-embedding-xyz","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestBuildUpstreamRequest_SetsContentType(t *testing.T) {
	var gotCT string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer test-token")
	p.ServeHTTP(httptest.NewRecorder(), req)

	if gotCT != "application/json" {
		t.Fatalf("expected Content-Type=application/json, got %q", gotCT)
	}
}

func TestHandleResponses_GET_ByID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/responses/resp_123" {
			t.Errorf("expected /v1/responses/resp_123, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":     "resp_123",
			"status": "completed",
		})
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	req := httptest.NewRequest(http.MethodGet, "/v1/responses/resp_123", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestHandleResponses_Cancel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/responses/resp_456/cancel" {
			t.Errorf("expected /v1/responses/resp_456/cancel, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "resp_456",
			// OpenAI Responses API wire status (spec enum uses the British spelling).
			"status": "cancelled", //nolint:misspell
		})
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/resp_456/cancel", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"cancelled"`) { //nolint:misspell // OpenAI wire status
		t.Fatalf("expected wire status pass-through, got body %q", rec.Body.String())
	}
}

func TestHandleResponses_Delete(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/v1/responses/resp_789" {
			t.Errorf("expected /v1/responses/resp_789, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	req := httptest.NewRequest(http.MethodDelete, "/v1/responses/resp_789", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", rec.Code)
	}
}

func TestHandleResponses_Compact(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/responses/resp_abc/compact" {
			t.Errorf("expected /v1/responses/resp_abc/compact, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":     "resp_abc",
			"status": "in_progress",
		})
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/resp_abc/compact", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestHandleResponses_PathTraversal(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "/v1/responses/../../etc/passwd", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for path traversal, got %d", rec.Code)
	}
}
