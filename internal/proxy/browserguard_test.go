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

func newBrowserGuardProxy(t *testing.T, allowed []string) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(600)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(10000000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Governance.AllowedBrowserOrigins = allowed
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider-0": {URL: "http://127.0.0.1:1/v1/chat/completions", AuthStyle: "none"},
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
	return p
}

func guardRequest(p *Proxy, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test-token")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// TestBrowserGuard_ReboundOriginRejected pins the GHSA lesson: an attacker
// page whose rebound hostname matches the request Host (so naive
// same-origin comparisons pass) is still denied — the allowlist, not the
// request's own Host, decides.
func TestBrowserGuard_ReboundOriginRejected(t *testing.T) {
	p := newBrowserGuardProxy(t, nil)
	rec := guardRequest(p, http.MethodPost, "/v1/chat/completions", map[string]string{
		"Origin": "http://127.0.0.1:4010",
		"Host":   "127.0.0.1:4010",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for rebound origin, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestBrowserGuard_AllowlistedOriginPasses checks explicit opt-in works,
// including default-port canonicalization.
func TestBrowserGuard_AllowlistedOriginPasses(t *testing.T) {
	// The upstream is unreachable (port 1), so a pass means the guard
	// released the request (502/503-class), not 403.
	p := newBrowserGuardProxy(t, []string{"http://localhost"})
	rec := guardRequest(p, http.MethodPost, "/v1/chat/completions", map[string]string{
		"Origin": "http://localhost:80", // default port canonicalizes away
	})
	if rec.Code == http.StatusForbidden {
		t.Fatalf("guard should not have blocked; got 403: %s", rec.Body.String())
	}

	p2 := newBrowserGuardProxy(t, []string{"https://studio.example.com"})
	rec2 := guardRequest(p2, http.MethodPost, "/v1/chat/completions", map[string]string{
		"Origin": "https://studio.example.com",
	})
	if rec2.Code == http.StatusForbidden {
		t.Fatalf("allowlisted origin must pass, got 403: %s", rec2.Body.String())
	}
}

// TestBrowserGuard_NoMetadataUnaffected checks curl/opencode-style requests
// without any fetch metadata are untouched.
func TestBrowserGuard_NoMetadataUnaffected(t *testing.T) {
	p := newBrowserGuardProxy(t, nil)
	rec := guardRequest(p, http.MethodGet, "/healthz", nil)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("metadata-free request must not be blocked, got 403")
	}
}

// TestBrowserGuard_GETSurfacesCovered pins that the no-auth GET surfaces —
// the exact reads a rebound page can attempt — are covered, including
// origin-less Sec-Fetch hints.
func TestBrowserGuard_GETSurfacesCovered(t *testing.T) {
	p := newBrowserGuardProxy(t, nil)
	for _, path := range []string{"/healthz", "/statsz", "/metrics"} {
		for _, headers := range []map[string]string{
			{"Origin": "https://evil.example"},
			{"Sec-Fetch-Site": "same-origin"}, // rebinding passes naive checks
			{"Sec-Fetch-Mode": "cors"},
		} {
			rec := guardRequest(p, http.MethodGet, path, headers)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s with %v: expected 403, got %d", path, headers, rec.Code)
			}
		}
	}
}

// TestCanonicalOrigin pins the normalization rules: lowercased scheme/host,
// default ports stripped, non-URL entries lowercased literally.
func TestCanonicalOrigin(t *testing.T) {
	tests := []struct{ in, want string }{
		{"http://LOCALHOST:80", "http://localhost"},
		{"https://Studio.Example.com:443", "https://studio.example.com"},
		{"http://localhost:3000", "http://localhost:3000"},
		{"https://a.example", "https://a.example"},
		{"null", "null"},
	}
	for _, tt := range tests {
		if got := canonicalOrigin(tt.in); got != tt.want {
			t.Errorf("canonicalOrigin(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
