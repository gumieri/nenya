package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nenya/config"
	"nenya/internal/gateway"
	"nenya/internal/testutil"
)

// FuzzChatHandler fuzzes the chat completion handler with random JSON payloads.
// This helps detect panics or malformed responses from edge case inputs.
func FuzzChatHandler(f *testing.F) {
	// Seed corpus with valid JSON inputs
	f.Add(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`)
	f.Add(`{"model":"test-model","messages":[{"role":"user","content":""}],"stream":true}`)
	f.Add(`{"messages":[]}`) // Empty messages
	f.Add(`{"model":""}`)   // Empty model
	
	f.Fuzz(func(t *testing.T, jsonData string) {
		t.Helper()
		
		// Skip invalid JSON to focus on logic errors
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(jsonData), &raw); err != nil {
			return
		}
		
		// Create test setup
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
		}))
		defer upstream.Close()

		cfg := testutil.MinimalConfig()
		cfg.Server.MaxBodyBytes = 10 << 20
		cfg.Governance.RatelimitMaxRPM = config.PtrTo(60)
		cfg.Governance.RatelimitMaxTPM = config.PtrTo(100000)
		cfg.Bouncer.Enabled = config.PtrTo(false)
		cfg.Providers = map[string]config.ProviderConfig{
			"test-provider": {
				URL:       upstream.URL + "/v1/chat/completions",
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
		defer gw.Close()
		p := &Proxy{}
		p.StoreGateway(gw)
		
		// Create request
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(jsonData))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		
		// Execute handler - should not panic
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		
		// Basic validation - should return either success or client error (not 500 unless truly invalid)
		if rr.Code >= 500 && rr.Code < 600 {
			// Only fail if it's a server error - client errors (4xx) are expected for bad input
			t.Errorf("Unexpected server error: %d, body: %s", rr.Code, rr.Body.String())
		}
	})
}