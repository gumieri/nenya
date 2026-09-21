package proxy

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
)

// enforceBrowserGuard implements the DNS-rebinding hardening (NENYA-34,
// evidence GHSA-2qr4-ww3x-mq4g): nenya is a localhost API serving
// non-browser clients, so any request carrying browser fetch metadata —
// the Origin header or Sec-Fetch-* hints, which page JavaScript cannot
// forge — is evidence of a browser context and is refused unless the
// origin is explicitly allowlisted via governance.allowed_browser_origins.
//
// The cross-origin checks a naive implementation would use (Sec-Fetch-Site
// same-origin; Origin.Host == Host) cannot detect DNS rebinding: a rebound
// hostname satisfies both. That is why the comparison is against an
// operator allowlist, never against the request's own Host, and why
// metadata-bearing requests are denied on every method including the
// no-auth GET surfaces (/healthz, /statsz, /metrics) — the exact reads a
// rebound page can attempt.
//
// Requests without any fetch metadata (curl, opencode, IDE clients) are
// unaffected. Returns true when the request was blocked (response
// written), false when it may proceed.
func (p *Proxy) enforceBrowserGuard(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	site := r.Header.Get("Sec-Fetch-Site")
	mode := r.Header.Get("Sec-Fetch-Mode")
	if origin == "" && site == "" && mode == "" {
		return false // no browser metadata: curl/opencode/IDE clients
	}

	allowed := gw.Config.Governance.AllowedBrowserOrigins
	if origin != "" {
		canonical := canonicalOrigin(origin)
		for _, e := range allowed {
			if canonicalOrigin(e) == canonical {
				return false
			}
		}
	}
	// Origin-less browser hints (GET navigations, stream fetches) cannot be
	// attributed to an allowlisted origin — default-deny.
	gw.Logger.Warn("browser-origin request rejected", "origin", origin, "site", site, "mode", mode, "path", r.URL.Path, "method", r.Method)
	writeStructuredError(w, http.StatusForbidden, infra.ErrorKindAuthFailed, "browser-origin requests are not allowed to this endpoint")
	return true
}

// canonicalOrigin normalizes an origin (or allowlist entry) for
// comparison: lowercased scheme and host, default ports (http/80, https/443)
// stripped. Entries that are not parseable URLs are compared as
// lowercased literals, so operators can allowlist non-URL sentinels like
// "null" if they truly intend to.
func canonicalOrigin(s string) string {
	s = strings.TrimSpace(s)
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return strings.ToLower(s)
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	switch {
	case port == "80" && u.Scheme == "http":
	case port == "443" && u.Scheme == "https":
	default:
		if port != "" {
			return u.Scheme + "://" + host + ":" + port
		}
	}
	return u.Scheme + "://" + host
}
