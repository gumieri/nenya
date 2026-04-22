package testutil

import (
	"log/slog"
	"net/http/httptest"
	"testing"

	"nenya/internal/config"
	"nenya/internal/gateway"
	"nenya/internal/proxy"
)

// NewTestProxy creates a test proxy with a real gateway.
// The proxy is ready to serve HTTP requests.
// Returns the proxy and an optional test server.
func NewTestProxy(t *testing.T, opts ...ConfigOption) (*proxy.Proxy, *httptest.Server) {
	t.Helper()

	cfg := TestConfig(opts...)
	secrets := &config.SecretsConfig{
		ClientToken:  "test-client-token",
		ProviderKeys: map[string]string{},
	}
	gw := gateway.New(*cfg, secrets, slog.Default())
	p := &proxy.Proxy{}
	p.StoreGateway(gw)
	return p, nil
}

// NewTestProxyWithHandler returns a test proxy wrapped in an httptest.Server.
// This is useful for integration tests that need a real HTTP endpoint.
func NewTestProxyWithHandler(t *testing.T, opts ...ConfigOption) (*proxy.Proxy, *httptest.Server) {
	t.Helper()

	p, _ := NewTestProxy(t, opts...)
	srv := httptest.NewServer(p)
	return p, srv
}

// NewTestGateway creates a minimal test gateway for unit tests.
// This bypasses the full gateway initialization and is useful
// for testing individual components that depend on a gateway.
func NewTestGateway(t *testing.T, opts ...ConfigOption) *gateway.NenyaGateway {
	t.Helper()

	cfg := TestConfig(opts...)
	secrets := &config.SecretsConfig{
		ClientToken:  "test-client-token",
		ProviderKeys: map[string]string{},
	}
	// Create a minimal gateway without full initialization
	return &gateway.NenyaGateway{
		Config:  *cfg,
		Secrets: secrets,
		Logger:  slog.Default(),
		Client:  NewTestHTTPClient(),
	}
}

// NewTestProxyFromGateway creates a test proxy from an existing gateway.
// Useful for testing proxy behavior with a pre-configured gateway.
func NewTestProxyFromGateway(t *testing.T, gw *gateway.NenyaGateway) *proxy.Proxy {
	t.Helper()

	p := &proxy.Proxy{}
	p.StoreGateway(gw)
	return p
}
