package gateway

import (
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/netutil"
	"github.com/nenya/internal/testutil"
)

// TestProviderNetworkPolicyResolution verifies provider-over-global
// resolution of the NENYA-137 network settings.
func TestProviderNetworkPolicyResolution(t *testing.T) {
	ca := testutil.TestCABundle(t)
	network := &config.NetworkConfig{CABundle: ca, ProxyURL: "http://egress:3128"}

	t.Run("provider inherits globals", func(t *testing.T) {
		p := &config.Provider{}
		got := providerNetworkPolicy(p, network)
		if got.caBundlePath != ca || got.proxyURL != "http://egress:3128" {
			t.Fatalf("got %+v, want globals from %+v", got, network)
		}
	})
	t.Run("provider overrides win", func(t *testing.T) {
		p := &config.Provider{CABundle: ca, ProxyURL: "socks5://private:1080"}
		got := providerNetworkPolicy(p, network)
		if got.proxyURL != "socks5://private:1080" {
			t.Fatalf("provider proxy override lost: %+v", got)
		}
	})
	t.Run("empty network yields environment policy", func(t *testing.T) {
		got := providerNetworkPolicy(&config.Provider{}, nil)
		if got != (networkPolicy{}) {
			t.Fatalf("expected zero policy, got %+v", got)
		}
	})
	t.Run("ollama never inherits the global proxy", func(t *testing.T) {
		p := &config.Provider{CABundle: ca}
		got := providerEffectivePolicy("ollama", p, network)
		if got.proxyURL != "" {
			t.Fatalf("ollama must not inherit the fleet proxy, got %q", got.proxyURL)
		}
		if got.caBundlePath != ca {
			t.Errorf("ollama explicit ca_bundle override expected, got %q", got.caBundlePath)
		}
	})
}

// TestApplyNetworkPolicy verifies the transport mutations: TLS floor, CA
// pool from the bundle, and the explicit proxy function.
func TestApplyNetworkPolicy(t *testing.T) {
	ca := testutil.TestCABundle(t)
	global, err := globalNetworkPolicy(&config.NetworkConfig{CABundle: ca, ProxyURL: "http://egress:3128"})
	if err != nil {
		t.Fatalf("globalNetworkPolicy: %v", err)
	}
	tr := &http.Transport{}
	global.apply(tr)
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != netutil.MinTLSVersion {
		t.Fatalf("TLS floor not applied: %#v", tr.TLSClientConfig)
	}
	if tr.TLSClientConfig.RootCAs == nil {
		t.Fatal("CA pool not applied")
	}
	if tr.Proxy == nil {
		t.Fatal("proxy func not applied")
	}

	t.Run("invalid bundle errors at load", func(t *testing.T) {
		if _, err := globalNetworkPolicy(&config.NetworkConfig{CABundle: filepath.Join(t.TempDir(), "missing.pem")}); err == nil {
			t.Fatal("expected error for missing bundle")
		}
	})
	t.Run("invalid proxy is caught by startup validation, not load", func(t *testing.T) {
		// Proxy scheme validation lives in config.ValidateNetworkPolicy;
		// globalNetworkPolicy degrades to direct egress for direct-built
		// configs (apply ignores the invalid URL).
		global, err := globalNetworkPolicy(&config.NetworkConfig{ProxyURL: "gopher://egress"})
		if err != nil {
			t.Fatalf("unexpected load error: %v", err)
		}
		tr := &http.Transport{}
		global.apply(tr)
		if tr.Proxy != nil {
			t.Error("invalid proxy URL must not be applied")
		}
	})
}

// TestBuildProviderClientsNetworkPolicy verifies that a provider with a
// custom CA bundle gets a dedicated client even when its timeouts match the
// fleet defaults, and that its transport carries the CA pool.
func TestBuildProviderClientsNetworkPolicy(t *testing.T) {
	ca := testutil.TestCABundle(t)
	base := newUpstreamTransport(time.Second)
	providers := map[string]*config.Provider{
		"custom-ca": {Name: "custom-ca", CABundle: ca},
		"vanilla":   {Name: "vanilla"},
	}
	clients := buildProviderClients(base, providers, networkPolicy{}, nil, slog.Default())
	if _, ok := clients["custom-ca"]; !ok {
		t.Fatal("expected a dedicated client for the provider with a CA bundle")
	}
	if _, ok := clients["vanilla"]; ok {
		t.Fatal("vanilla provider must reuse the base transport (no dedicated client)")
	}
	caClient := clients["custom-ca"]
	tr, ok := caClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", caClient.Transport)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
		t.Fatal("dedicated client transport missing the CA pool")
	}
}

// TestBuildProviderClientsInvalidPolicy verifies the skip-and-log path: a
// provider whose CA bundle cannot load loses its dedicated client (falls
// back to the base transport) instead of failing the whole build.
func TestBuildProviderClientsInvalidPolicy(t *testing.T) {
	base := newUpstreamTransport(time.Second)
	providers := map[string]*config.Provider{
		"broken":  {Name: "broken", CABundle: filepath.Join(t.TempDir(), "missing.pem")},
		"timeout": {Name: "timeout", IdleConnTimeoutSeconds: 5},
	}
	clients := buildProviderClients(base, providers, networkPolicy{}, nil, slog.Default())
	if _, ok := clients["broken"]; ok {
		t.Error("provider with invalid CA bundle must not get a dedicated client")
	}
	if _, ok := clients["timeout"]; !ok {
		t.Error("unrelated timeout-differentiated provider must keep its dedicated client")
	}
}
