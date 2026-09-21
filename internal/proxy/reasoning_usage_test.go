package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/testutil"
)

// TestNonStreamingReasoningTokensRecorded pins NENYA-33 end to end: a
// non-streaming response whose usage carries only the nested OpenAI
// reasoning details still lands in /statsz reasoning_tokens (previously
// the non-stream path had no reasoning extraction at all).
func TestNonStreamingReasoningTokensRecorded(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"output_tokens_details":{"reasoning_tokens":12}}}`))
	}))
	defer up.Close()

	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(600)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(10000000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider-0": {URL: up.URL + "/v1/chat/completions", AuthStyle: "none"},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models:   []config.AgentModel{{Provider: "test-provider-0", Model: "test-model"}},
		},
	}
	secrets := &config.SecretsConfig{ClientToken: "test-token"}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p := &Proxy{}
	p.StoreGateway(gw)

	body := `{"model":"test-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	snapshot := gw.Stats.Snapshot()
	models, ok := snapshot["models"].(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected stats snapshot shape: %v", snapshot)
	}
	ms, ok := models["test-model"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected test-model stats, got %v", models)
	}
	reasoning, _ := ms["reasoning_tokens"].(uint64)
	if reasoning != 12 {
		t.Errorf("expected reasoning_tokens=12 recorded for non-stream response, got %d (model stats: %v)", reasoning, ms)
	}
}
