package netutil

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
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

// RedactedUserinfoSentinel replaces credential-bearing userinfo in
// redacted URL output.
const RedactedUserinfoSentinel = "[REDACTED]"

// RedactURLUserinfo replaces the userinfo of a URL (proxy credentials are
// accepted in userinfo form by design) with a sentinel, leaving the rest of
// the URL visible. The strip is best-effort: it does not require the URL to
// parse, so a malformed string cannot smuggle a credential past it, and only
// an "@" that precedes the authority terminator (/ ? #) is treated as
// userinfo, so an "@" in a path or query is left alone. Credential-free
// inputs are returned unchanged. Shared by the diagnostic CLI surface and the
// request logger (NENYA-145): a first-party output channel must never echo a
// credential.
func RedactURLUserinfo(raw string) string {
	if raw == "" {
		return raw
	}
	prefix, rest := "", raw
	if sep := strings.Index(raw, "://"); sep >= 0 {
		prefix, rest = raw[:sep+3], raw[sep+3:]
	}
	// Only a credential-looking userinfo is redacted: an "@" in a path or a
	// query is not one. Everything before the "@" is replaced (over-redaction
	// is safe for a diagnostic), and the sentinel is inserted literally rather
	// than through url.User, which would percent-encode its brackets.
	at := strings.IndexByte(rest, '@')
	if at < 0 || strings.ContainsAny(rest[:at], "/?#") {
		return raw
	}
	return prefix + RedactedUserinfoSentinel + "@" + rest[at+1:]
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
