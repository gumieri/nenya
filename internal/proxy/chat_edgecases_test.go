package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nenya/config"
	"nenya/internal/gateway"
	"nenya/internal/testutil"
)

// TestEdgeCasePayloads tests the chat handler with various edge case inputs
// including empty bodies, malformed JSON, and large messages.
func TestEdgeCasePayloads(t *testing.T) {
	tests := []struct {
		name        string
		payload     string
		expectCode  int
		description string
	}{
		{
			name:        "empty messages array",
			payload:     `{"model":"test-agent","messages":[]}`,
			expectCode:  http.StatusOK,
			description: "Empty messages array accepted by upstream",
		},
		{
			name:        "missing model field",
			payload:     `{"messages":[{"role":"user","content":"test"}]}`,
			expectCode:  http.StatusBadRequest,
			description: "Missing model field should return 400",
		},
		{
			name:        "malformed JSON",
			payload:     `{"model":"test","messages":[{"role":"user","content":unclosed`,
			expectCode:  http.StatusBadRequest,
			description: "Malformed JSON should be rejected",
		},
		{
			name:        "empty content",
			payload:     `{"model":"test-agent","messages":[{"role":"user","content":""}]}`,
			expectCode:  http.StatusOK,
			description: "Empty content should be accepted",
		},
		{
			name:        "unicode content",
			payload:     `{"model":"test-agent","messages":[{"role":"user","content":"你好世界 🌍"}]}`,
			expectCode:  http.StatusOK,
			description: "Unicode should be handled correctly",
		},
		{
			name:        "empty payload",
			payload:     `{}`,
			expectCode:  http.StatusBadRequest,
			description: "Empty object should return 400",
		},
		{
			name:        "extra fields ignored",
			payload:     `{"model":"test-agent","messages":[{"role":"user","content":"hi"}],"extra_field":true}`,
			expectCode:  http.StatusOK,
			description: "Extra fields should be accepted",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
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

			// Execute
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tt.payload))
			req.Header.Set("Authorization", "Bearer test-token")
			req.Header.Set("Content-Type", "application/json")

			rr := httptest.NewRecorder()
			p.ServeHTTP(rr, req)

			// Verify
			if rr.Code != tt.expectCode {
				t.Errorf("%s: expected status %d, got %d", tt.description, tt.expectCode, rr.Code)
				t.Logf("Response body: %s", rr.Body.String())
			}
		})
	}
}
