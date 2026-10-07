package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/netutil"
)

// networkPolicy is the resolved outbound transport policy for one scope
// (global default or a single provider): a private CA bundle path and an
// egress proxy URL. Zero value = system roots + HTTPS_PROXY/NO_PROXY
// environment (NENYA-137). pool holds the loaded CA bundle so repeated
// applies (base + per-provider clones) read the PEM once.
type networkPolicy struct {
	caBundlePath string
	proxyURL     string
	pool         *x509.CertPool
}

// globalNetworkPolicy reads the fleet-wide defaults, loading the CA bundle
// once into the policy.
func globalNetworkPolicy(network *config.NetworkConfig) (networkPolicy, error) {
	p := providerNetworkPolicy(nil, network)
	if p.caBundlePath != "" {
		pool, err := netutil.LoadCABundle(p.caBundlePath)
		if err != nil {
			return p, err
		}
		p.pool = pool
	}
	return p, nil
}

// providerEffectivePolicy resolves a provider's policy for transport
// building. Ollama-format providers (by registry name or api_format) are
// special-cased on the proxy only: the fleet-wide proxy is not inherited —
// it would route loopback Ollama through the corporate egress (the
// environment default never proxies loopback). The fleet-wide CA bundle
// IS inherited for ollama: an extra root on a loopback endpoint is harmless
// trust, and dropping it would break a remote Ollama behind the private CA.
func providerEffectivePolicy(name string, p *config.Provider, network *config.NetworkConfig) networkPolicy {
	policy := providerNetworkPolicy(p, network)
	if p != nil && isOllamaProvider(name, p) {
		policy.proxyURL = p.ProxyURL
	}
	return policy
}

// isOllamaProvider reports whether a provider is an Ollama engine, either
// by registry name or by declared wire format.
func isOllamaProvider(name string, p *config.Provider) bool {
	if p == nil {
		return strings.EqualFold(name, "ollama")
	}
	return strings.EqualFold(name, "ollama") || strings.EqualFold(p.ApiFormat, "ollama")
}

// providerNetworkPolicy resolves a provider's effective policy: per-provider
// overrides over the network-level globals.
func providerNetworkPolicy(p *config.Provider, network *config.NetworkConfig) networkPolicy {
	return networkPolicy{
		caBundlePath: p.EffectiveCABundle(network),
		proxyURL:     p.EffectiveProxyURL(network),
	}
}

// apply mutates the transport in place: explicit TLS 1.2 floor (+HTTP/2
// via ForceAttemptHTTP2, since a non-nil TLSClientConfig disables the
// automatic upgrade), the CA pool from the bundle (appended to system
// roots), and the proxy function (explicit URL, else environment). Callers
// must have loaded the pool (globalNetworkPolicy, loadPool, or
// buildProviderClients' cache); apply never fails and always leaves the transport on system defaults +
// proxy environment for the parts it could not apply.
func (p networkPolicy) apply(t *http.Transport) {
	if t == nil {
		return
	}
	netutil.ApplyTransportSecurity(t)

	if p.caBundlePath != "" {
		if t.TLSClientConfig == nil {
			t.TLSClientConfig = &tls.Config{MinVersion: netutil.MinTLSVersion}
		}
		if p.pool != nil {
			t.TLSClientConfig.RootCAs = p.pool
		}
	}
	if proxy, err := netutil.ProxyOrDefault(p.proxyURL); err == nil {
		t.Proxy = proxy
	}
	// Invalid proxy URLs are caught by config validation before this point;
	// a direct-built config without validation keeps direct egress.
}

// loadPool loads the CA bundle for policies carrying a bundle path without
// a pool (the ollama policy built in createHTTPClients), degrading to a
// bundle-less policy (logged) when the file cannot be read — startup
// validation rejects unreadable bundles; this is the direct-built-config
// safety net. Returns the policy to use.
func (p networkPolicy) loadPool(logger *slog.Logger) networkPolicy {
	if p.caBundlePath == "" || p.pool != nil {
		return p
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	pool, err := netutil.LoadCABundle(p.caBundlePath)
	if err != nil {
		logger.Error("ca_bundle invalid — CA override dropped", "path", p.caBundlePath, "err", err)
		p.caBundlePath = ""
		return p
	}
	p.pool = pool
	return p
}

// loadProviderPool returns the pool for the provider's CA bundle path,
// loading and caching it (failures are remembered as a nil entry so a bad
// path is logged once, not per provider sharing it). ok=false means the
// provider must keep the fleet-default transport.
func loadProviderPool(path string, loadedPools map[string]*x509.CertPool, logger *slog.Logger, providerName string) (*x509.CertPool, bool) {
	if pool, cached := loadedPools[path]; cached {
		return pool, pool != nil
	}
	pool, err := netutil.LoadCABundle(path)
	if err != nil {
		logger.Error("provider CA bundle invalid — provider keeps the fleet-default transport", "provider", providerName, "err", err)
		loadedPools[path] = nil // remember the failure
		return nil, false
	}
	loadedPools[path] = pool
	return pool, true
}

// buildProviderClients clones the base upstream transport for every provider
// whose effective response-header timeout, idle-connection timeout, or
// network policy (NENYA-137: CA bundle / egress proxy) differs from the
// fleet defaults, yielding one dedicated client per provider name. Providers
// sharing all defaults reuse the base transport's connection pool via the
// shared client. A provider whose CA bundle cannot load loses its dedicated
// client (falls back to the base transport's policy) with a logged error —
// config validation fails startup for such configs; this is the
// direct-built-config safety net. global is the fleet policy resolved once
// by the caller (New) so the CA bundle is read a single time per start.
func buildProviderClients(baseTransport *http.Transport, providers map[string]*config.Provider, global networkPolicy, network *config.NetworkConfig, logger *slog.Logger) map[string]*http.Client {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	loadedPools := map[string]*x509.CertPool{global.caBundlePath: global.pool}

	clients := make(map[string]*http.Client)
	for name, provider := range providers {
		if provider == nil {
			continue
		}
		headerTimeout := provider.EffectiveResponseHeaderTimeout()
		idleTimeout := provider.EffectiveIdleConnTimeout()
		policy := providerEffectivePolicy(name, provider, network)
		hasExplicitOverrides := provider.CABundle != "" || provider.ProxyURL != ""
		if !hasExplicitOverrides &&
			headerTimeout == config.DefaultResponseHeaderTimeoutSeconds*time.Second &&
			idleTimeout == config.DefaultIdleConnTimeoutSeconds*time.Second &&
			policy.caBundlePath == global.caBundlePath && policy.proxyURL == global.proxyURL {
			continue
		}

		if policy.caBundlePath != "" && policy.pool == nil {
			pool, ok := loadProviderPool(policy.caBundlePath, loadedPools, logger, name)
			if !ok {
				continue
			}
			policy.pool = pool
		}

		transport := baseTransport.Clone()
		transport.ResponseHeaderTimeout = headerTimeout
		transport.IdleConnTimeout = idleTimeout
		policy.apply(transport)
		clients[name] = &http.Client{Transport: transport}
	}
	return clients
}
