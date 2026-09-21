package proxy

import (
	"context"
	"encoding/json"
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

// newParamRetryProxy builds a single-target proxy with the param-reject
// safety net toggled per enable (NENYA-32). No param-compat rule matches
// the test model: the proactive table stays out of the picture, so the
// reactive net is exercised in isolation. chainSize controls how many
// identical provider entries the agent chain carries.
func newParamRetryProxy(t *testing.T, upstreamURL string, enable bool, chainSize int) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(600)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(10000000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Governance.AutoRetryOnParamReject = config.PtrTo(enable)
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider-0": {URL: upstreamURL + "/v1/chat/completions", AuthStyle: "none"},
	}
	models := make([]config.AgentModel, 0, chainSize)
	for i := 0; i < chainSize; i++ {
		models = append(models, config.AgentModel{Provider: "test-provider-0", Model: "test-model"})
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			// Corrective retries consume the next sweep slot (same
			// semantics as the context-limit summarization retry), so the
			// positive-case chain carries the entry twice — the documented
			// failover-within-provider pattern.
			Models: models,
		},
	}
	secrets := &config.SecretsConfig{ClientToken: "test-token"}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

func postChatJSON(t *testing.T, p *Proxy, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// TestParamRejectStripAndRetry pins NENYA-32's reactive safety net: a
// model outside the compat table rejects an unexpected parameter with a
// 400 naming it; with auto_retry_on_param_reject the gateway strips the
// named parameter and succeeds on the retry.
func TestParamRejectStripAndRetry(t *testing.T) {
	var bodies []string
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"Unrecognized request argument supplied: temperature"}}`))
			return
		}
		bodies = append(bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()

	p := newParamRetryProxy(t, up.URL, true, 2)
	rec := postChatJSON(t, p, `{"model":"test-agent","stream":false,"temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected retry to succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected exactly 2 upstream calls, got %d", got)
	}
	if len(bodies) != 1 {
		t.Fatalf("expected 1 captured success body, got %d", len(bodies))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["temperature"]; ok {
		t.Error("retry payload must not carry the rejected parameter")
	}
	if payload["messages"] == nil {
		t.Error("retry payload must keep the conversation")
	}
}

// TestParamRejectDisabledSurfacesError checks the net is opt-in: disabled,
// the 400 surfaces to the client unchanged and no second call happens.
func TestParamRejectDisabledSurfacesError(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Unrecognized request argument supplied: temperature"}}`))
	}))
	defer up.Close()

	p := newParamRetryProxy(t, up.URL, false, 1)
	rec := postChatJSON(t, p, `{"model":"test-agent","stream":false,"temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 to surface, got %d", rec.Code)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", got)
	}
}

// TestParamRejectUnknownBodyNotRetried checks conservatism: a 400 whose
// body names no known chat parameter never triggers the net.
func TestParamRejectUnknownBodyNotRetried(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"total nonsense failure"}}`))
	}))
	defer up.Close()

	p := newParamRetryProxy(t, up.URL, true, 1)
	rec := postChatJSON(t, p, `{"model":"test-agent","stream":false,"temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 to surface, got %d", rec.Code)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", got)
	}
}
