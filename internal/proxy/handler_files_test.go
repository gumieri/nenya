package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"nenya/config"
	"nenya/internal/gateway"
	"nenya/internal/infra"
)

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func TestRouteHandler_PathTraversal(t *testing.T) {
	cfg := config.Config{
		Server: config.ServerConfig{
			MaxBodyBytes: 10 * 1024 * 1024,
		},
		Governance: config.GovernanceConfig{
			RatelimitMaxRPM: config.PtrTo(10),
			RatelimitMaxTPM: config.PtrTo(10000),
		},
	}

	providers := make(map[string]*config.Provider)
	providers["openai"] = &config.Provider{
		Name:           "openai",
		URL:            "https://api.openai.com/v1/chat/completions",
		BaseURL:        "https://api.openai.com",
		APIKey:         "test-key",
		AuthStyle:      "bearer",
		TimeoutSeconds: 30,
	}

	logger := infra.SetupLogger(false)
	gw := &gateway.NenyaGateway{
		Config:      cfg,
		Secrets:     &config.SecretsConfig{ClientToken: "client-token"},
		Client:      http.DefaultClient,
		Providers:   providers,
		RateLimiter: infra.NewRateLimiter(derefInt(cfg.Governance.RatelimitMaxRPM), derefInt(cfg.Governance.RatelimitMaxTPM)),
		Stats:       infra.NewUsageTracker(),
		Logger:      logger,
	}

	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	tests := []struct {
		name     string
		path     string
		method   string
		expected int
	}{
		{"files path traversal", "/v1/files/../", "GET", http.StatusBadRequest},
		{"batches path traversal", "/v1/batches/../", "POST", http.StatusBadRequest},
		{"files with .. in path", "/v1/files/..%2Fsecret", "GET", http.StatusBadRequest},
		{"batches with .. in path", "/v1/batches/..%2Fsecret", "POST", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("Authorization", "Bearer client-token")
			w := httptest.NewRecorder()
			proxy.ServeHTTP(w, req)

			if w.Code != tt.expected {
				t.Errorf("expected status %d, got %d", tt.expected, w.Code)
			}
		})
	}
}
