package netutil

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
)

// MinTLSVersion is the explicit client-side TLS floor applied to every
// outbound transport. Go's default already rejects anything older in
// practice, but pinning it makes the policy explicit and auditable
// (NENYA-137).
const MinTLSVersion = tls.VersionTLS12

// ApplyTransportSecurity pins the outbound TLS floor on a transport.
// Idempotent; preserves any TLSClientConfig already set (only MinVersion
// is forced up to MinTLSVersion).
func ApplyTransportSecurity(t *http.Transport) {
	if t == nil {
		return
	}
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{}
	}
	if t.TLSClientConfig.MinVersion < MinTLSVersion {
		t.TLSClientConfig.MinVersion = MinTLSVersion
	}
	// A non-nil TLSClientConfig disables the automatic HTTP/2 upgrade —
	// re-enable it explicitly so the floor does not cost h2.
	t.ForceAttemptHTTP2 = true
}

// LoadCABundle reads a PEM-encoded certificate bundle from path and returns
// a cert pool containing the system roots plus the bundled certificates, so
// a private CA grants trust without disabling system trust.
func LoadCABundle(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, fmt.Errorf("ca bundle path is empty")
	}
	pem, err := os.ReadFile(path) // #nosec G304 -- path comes from validated operator config
	if err != nil {
		return nil, fmt.Errorf("read ca bundle %q: %w", path, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		// System pool unavailable (rare); start from an empty pool.
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("ca bundle %q contains no parseable PEM certificates", path)
	}
	return pool, nil
}

// parseProxy parses and validates a configured egress proxy URL. Error
// messages never embed the raw URL: operator proxy URLs may embed
// credentials. Schemes http, https, socks5, and socks5h (DNS resolution
// via the proxy — supported by net/http transports) are accepted; anything
// else (or an unparseable URL) is rejected at startup rather than at first
// dial.
func parseProxy(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("proxy url is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy url (unparseable)")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy url is missing a host")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (want http, https, socks5, or socks5h)", u.Scheme)
	}
	return u, nil
}

// ValidateProxyURL checks a configured egress proxy URL, rejecting an
// unparseable URL, a missing host, or an unsupported scheme at startup
// rather than at first dial.
func ValidateProxyURL(raw string) error {
	_, err := parseProxy(raw)
	return err
}

// ProxyOrDefault returns a transport Proxy function honoring the explicit
// proxy URL when set, falling back to the standard environment variables
// (HTTPS_PROXY/NO_PROXY) — the enterprise-egress default. An invalid
// explicit URL returns an error; validate at startup via ValidateProxyURL.
func ProxyOrDefault(proxyURL string) (func(*http.Request) (*url.URL, error), error) {
	if proxyURL == "" {
		return http.ProxyFromEnvironment, nil
	}
	u, err := parseProxy(proxyURL)
	if err != nil {
		return nil, err
	}
	return http.ProxyURL(u), nil
}
