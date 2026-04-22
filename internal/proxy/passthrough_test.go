package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nenya/internal/config"
	"nenya/internal/gateway"
	"nenya/internal/infra"
)

func TestHandlePassthrough(t *testing.T) {
	tests := []struct {
		name           string
		path           string
		method         string
		body           string
		providerName   string
		providerURL    string
		providerKey    string
		authStyle      string
		expectStatus   int
		expectBody     string
		upstreamStatus int
		upstreamBody   string
		upstreamSSE    bool
	}{
		{
			name:         "unknown provider",
			path:         "/proxy/unknown/v1/models",
			method:       "GET",
			providerName: "unknown",
			expectStatus: http.StatusNotFound,
			expectBody:   "Unknown provider",
		},
		{
			name:         "provider not configured",
			path:         "/proxy/anthropic/v1/models",
			method:       "GET",
			providerName: "anthropic",
			providerURL:  "https://api.anthropic.com/v1/messages",
			authStyle:    "bearer",
			expectStatus: http.StatusServiceUnavailable,
			expectBody:   "Provider not configured",
		},
		{
			name:         "missing endpoint path",
			path:         "/proxy/anthropic",
			method:       "GET",
			providerName: "anthropic",
			providerURL:  "https://api.anthropic.com/v1/messages",
			providerKey:  "test-key",
			authStyle:    "bearer",
			expectStatus: http.StatusBadRequest,
			expectBody:   "Missing endpoint path",
		},
		{
			name:         "path traversal attempt",
			path:         "/proxy/anthropic/../etc/passwd",
			method:       "GET",
			providerName: "anthropic",
			providerURL:  "https://api.anthropic.com/v1/messages",
			providerKey:  "test-key",
			authStyle:    "bearer",
			expectStatus: http.StatusBadRequest,
			expectBody:   "Invalid path",
		},
		{
			name:           "GET passthrough",
			path:           "/proxy/anthropic/v1/models",
			method:         "GET",
			providerName:   "anthropic",
			providerURL:    "https://api.anthropic.com/v1/messages",
			providerKey:    "test-key",
			authStyle:      "anthropic",
			expectStatus:   http.StatusOK,
			upstreamStatus: http.StatusOK,
			upstreamBody:   `{"data":[{"id":"claude-3-5-sonnet"}]}`,
		},
		{
			name:           "POST passthrough",
			path:           "/proxy/openai/v1/files",
			method:         "POST",
			body:           `{"purpose":"fine-tune"}`,
			providerName:   "openai",
			providerURL:    "https://api.openai.com/v1/chat/completions",
			providerKey:    "test-key",
			authStyle:      "bearer",
			expectStatus:   http.StatusOK,
			upstreamStatus: http.StatusOK,
			upstreamBody:   `{"id":"file-123"}`,
		},
		{
			name:           "DELETE passthrough",
			path:           "/proxy/openai/v1/files/file-123",
			method:         "DELETE",
			providerName:   "openai",
			providerURL:    "https://api.openai.com/v1/chat/completions",
			providerKey:    "test-key",
			authStyle:      "bearer",
			expectStatus:   http.StatusOK,
			upstreamStatus: http.StatusOK,
			upstreamBody:   `{"deleted":true}`,
		},
		{
			name:           "query string preservation",
			path:           "/proxy/anthropic/v1/models?limit=10",
			method:         "GET",
			providerName:   "anthropic",
			providerURL:    "https://api.anthropic.com/v1/messages",
			providerKey:    "test-key",
			authStyle:      "anthropic",
			expectStatus:   http.StatusOK,
			upstreamStatus: http.StatusOK,
			upstreamBody:   `{"data":[]}`,
		},
		{
			name:           "SSE streaming",
			path:           "/proxy/anthropic/v1/messages",
			method:         "POST",
			body:           `{"model":"claude-3-5-sonnet","max_tokens":10}`,
			providerName:   "anthropic",
			providerURL:    "https://api.anthropic.com/v1/messages",
			providerKey:    "test-key",
			authStyle:      "anthropic",
			expectStatus:   http.StatusOK,
			upstreamStatus: http.StatusOK,
			upstreamSSE:    true,
			upstreamBody:   "data: {\"type\":\"message_delta\"}\n\ndata: {\"type\":\"message_stop\"}\n\n",
		},
		{
			name:         "rate limit exceeded",
			path:         "/proxy/anthropic/v1/models",
			method:       "GET",
			providerName: "anthropic",
			providerURL:  "https://api.anthropic.com/v1/messages",
			providerKey:  "test-key",
			authStyle:    "bearer",
			expectStatus: http.StatusTooManyRequests,
			expectBody:   "Rate limit exceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamHandler http.HandlerFunc
			if tt.upstreamStatus != 0 {
				upstreamHandler = func(w http.ResponseWriter, r *http.Request) {
					auth := r.Header.Get("Authorization")
					apiKey := r.Header.Get("x-api-key")
					if tt.authStyle == "bearer" && auth != "Bearer "+tt.providerKey {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					if tt.authStyle == "anthropic" && apiKey != tt.providerKey {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}

					if tt.upstreamSSE {
						w.Header().Set("Content-Type", "text/event-stream")
						w.WriteHeader(tt.upstreamStatus)
						_, _ = io.WriteString(w, tt.upstreamBody)
						return
					}

					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tt.upstreamStatus)
					_, _ = io.WriteString(w, tt.upstreamBody)
				}
			}

			upstreamServer := httptest.NewServer(upstreamHandler)
			defer upstreamServer.Close()

			if tt.providerURL != "" {
				tt.providerURL = upstreamServer.URL
			}

			cfg := config.Config{
				Server: config.ServerConfig{
					MaxBodyBytes: 10 * 1024 * 1024,
				},
				Governance: config.GovernanceConfig{
					RatelimitMaxRPM: 10,
					RatelimitMaxTPM: 10000,
				},
			}

			providers := make(map[string]*config.Provider)
			if tt.providerName != "" && tt.providerURL != "" {
				providers[tt.providerName] = &config.Provider{
					Name:           tt.providerName,
					URL:            tt.providerURL,
					BaseURL:        strings.TrimSuffix(tt.providerURL, "/v1/messages"),
					APIKey:         tt.providerKey,
					AuthStyle:      tt.authStyle,
					TimeoutSeconds: 30,
				}
			}

			gw := &gateway.NenyaGateway{
				Config:      cfg,
				Secrets:     &config.SecretsConfig{ClientToken: "client-token"},
				Client:      http.DefaultClient,
				Providers:   providers,
				RateLimiter: infra.NewRateLimiter(cfg.Governance.RatelimitMaxRPM, cfg.Governance.RatelimitMaxTPM),
				Stats:       infra.NewUsageTracker(),
				Logger:      infra.SetupLogger(false),
			}

			if tt.name == "rate limit exceeded" {
				for i := 0; i < 15; i++ {
					gw.RateLimiter.Check(tt.providerURL, 0)
				}
			}

			proxy := &Proxy{}
			proxy.StoreGateway(gw)

			var bodyReader io.Reader
			if tt.body != "" {
				bodyReader = strings.NewReader(tt.body)
			}

			req := httptest.NewRequest(tt.method, tt.path, bodyReader)
			req.Header.Set("Authorization", "Bearer client-token")
			if tt.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}

			w := httptest.NewRecorder()
			proxy.ServeHTTP(w, req)

			resp := w.Result()
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.expectStatus {
				t.Errorf("expected status %d, got %d", tt.expectStatus, resp.StatusCode)
			}

			if tt.expectBody != "" {
				body, _ := io.ReadAll(resp.Body)
				if !strings.Contains(string(body), tt.expectBody) {
					t.Errorf("expected body to contain %q, got %q", tt.expectBody, string(body))
				}
			}

			if tt.upstreamStatus != 0 && tt.expectStatus == http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				if tt.upstreamSSE {
					if !strings.Contains(string(body), "data:") {
						t.Errorf("expected SSE response, got %q", string(body))
					}
				} else if tt.upstreamBody != "" && string(body) != tt.upstreamBody {
					t.Errorf("expected body %q, got %q", tt.upstreamBody, string(body))
				}
			}
		})
	}
}

func TestPipeSSE(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple SSE",
			input:    "data: {\"type\":\"message\"}\n\ndata: {\"type\":\"stop\"}\n\n",
			expected: "data: {\"type\":\"message\"}\n\ndata: {\"type\":\"stop\"}\n\n",
		},
		{
			name:     "SSE with multiple events",
			input:    "data: chunk1\n\ndata: chunk2\n\ndata: chunk3\n\n",
			expected: "data: chunk1\n\ndata: chunk2\n\ndata: chunk3\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy := &Proxy{}
			logger := infra.SetupLogger(false)

			src := strings.NewReader(tt.input)
			w := httptest.NewRecorder()

			proxy.pipeSSE(logger, src, w)

			resp := w.Result()
			defer func() { _ = resp.Body.Close() }()

			body, _ := io.ReadAll(resp.Body)
			if string(body) != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, string(body))
			}
		})
	}
}

func TestBuildUpstreamRequest(t *testing.T) {
	cfg := config.Config{
		Server: config.ServerConfig{
			UserAgent: "nenya-test/1.0",
		},
	}

	providers := map[string]*config.Provider{
		"test": {
			Name:      "test",
			URL:       "https://api.test.com/v1/chat",
			BaseURL:   "https://api.test.com",
			APIKey:    "secret-key",
			AuthStyle: "bearer",
		},
	}

	gw := &gateway.NenyaGateway{
		Config:    cfg,
		Secrets:   &config.SecretsConfig{ClientToken: "client-token"},
		Providers: providers,
		Logger:    infra.SetupLogger(false),
	}

	proxy := &Proxy{}
	proxy.StoreGateway(gw)

	req := httptest.NewRequest("POST", "/proxy/test/v1/models", strings.NewReader(`{"test":true}`))
	req.Header.Set("X-Request-Id", "req-123")
	req.Header.Set("Content-Type", "application/json")

	upstreamReq, err := proxy.buildUpstreamRequest(gw, context.Background(), "POST", "https://api.test.com/v1/models", []byte(`{"test":true}`), "test", req.Header)
	if err != nil {
		t.Fatalf("failed to build upstream request: %v", err)
	}

	if upstreamReq.Header.Get("Authorization") != "Bearer secret-key" {
		t.Errorf("expected Authorization header 'Bearer secret-key', got %q", upstreamReq.Header.Get("Authorization"))
	}

	if upstreamReq.Header.Get("X-Request-Id") != "req-123" {
		t.Errorf("expected X-Request-Id 'req-123', got %q", upstreamReq.Header.Get("X-Request-Id"))
	}

	if upstreamReq.Header.Get("User-Agent") != "nenya-test/1.0" {
		t.Errorf("expected User-Agent 'nenya-test/1.0', got %q", upstreamReq.Header.Get("User-Agent"))
	}

	if upstreamReq.Header.Get("Accept-Encoding") != "identity" {
		t.Errorf("expected Accept-Encoding 'identity', got %q", upstreamReq.Header.Get("Accept-Encoding"))
	}

	if upstreamReq.Header.Get("Connection") != "" {
		t.Errorf("expected Connection header to be stripped, got %q", upstreamReq.Header.Get("Connection"))
	}

	body, _ := io.ReadAll(upstreamReq.Body)
	if string(body) != `{"test":true}` {
		t.Errorf("expected body %q, got %q", `{"test":true}`, string(body))
	}
}
