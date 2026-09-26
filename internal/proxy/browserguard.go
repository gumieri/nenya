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
// Origin-less requests carrying only Sec-Fetch-* hints (same-origin GET
// navigations, and Node/undici's automatic `Sec-Fetch-Mode: cors`) cannot
// be attributed to an allowlisted origin and are default-denied. That is a
// problem for non-browser HTTP clients that emit fetch metadata as a side
// effect (undici does), which would otherwise be indistinguishable from a
// rebound page. Operators can opt in with the sentinel entry "*" in
// governance.allowed_browser_origins, which allows Origin-less
// metadata-only requests. This is safe against rebinding specifically
// because a rebound page's cross-origin fetch always sends Origin — that
// path still consults the allowlist — while Origin-less Sec-Fetch hints
// are not the cross-origin vector.
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
			if e != allowOriginlessMetadata && canonicalOrigin(e) == canonical {
				return false
			}
		}
	} else if containsString(allowed, allowOriginlessMetadata) {
		// Explicit opt-in: allow Origin-less metadata-only requests (e.g.
		// Node/undici's automatic Sec-Fetch-Mode). A rebound page always
		// sends Origin on cross-origin fetches, so this does not reopen the
		// rebinding vector.
		return false
	}
	// Origin-less browser hints (GET navigations, stream fetches) cannot be
	// attributed to an allowlisted origin — default-deny.
	gw.Logger.Warn("browser-origin request rejected", "origin", origin, "site", site, "mode", mode, "path", r.URL.Path, "method", r.Method)
	writeStructuredError(w, http.StatusForbidden, infra.ErrorKindAuthFailed, "browser-origin requests are not allowed to this endpoint")
	return true
}

// allowOriginlessMetadata is the governance.allowed_browser_origins sentinel
// that opts in to Origin-less Sec-Fetch-* requests.
const allowOriginlessMetadata = "*"

// containsString reports whether s contains want.
func containsString(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
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
