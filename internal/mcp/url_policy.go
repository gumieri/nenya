package mcp

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/nenya/internal/netutil"
)

// checkURLs walks the argument tree and flags URL-shaped or destination-shaped
// string content hosted on denied destinations (private/loopback/link-local/
// CGNAT/NAT64 in every accepted spelling, localhost variants, .internal/.local
// suffixes) unless the host is allowlisted. Policy "off" returns nothing; "log"
// still collects flags so the caller can record them, but rejection is decided
// by ValidateArgs.
func checkURLs(node any, cfg ArgGuardConfig, path string) []ArgViolation {
	if cfg.URLPolicy == URLPolicyOff {
		return nil
	}
	var violations []ArgViolation
	var walk func(any, string, int)
	walk = func(n any, p string, depth int) {
		if depth > maxWalkDepth {
			violations = append(violations, ArgViolation{
				Path:   p,
				Reason: fmt.Sprintf("arguments nested beyond the %d level cap", maxWalkDepth),
			})
			return
		}
		switch v := n.(type) {
		case map[string]any:
			for key, child := range v {
				walk(child, joinPath(p, key), depth+1)
			}
		case []any:
			for i, child := range v {
				walk(child, fmt.Sprintf("%s[%d]", p, i), depth+1)
			}
		case string:
			if reason, denied := denyURLHost(v, cfg.AllowedHosts); denied {
				violations = append(violations, ArgViolation{Path: p, Reason: reason})
			}
		}
	}
	walk(node, path, 0)
	return violations
}

// maxWalkDepth caps argument-tree traversal, mirroring the schema
// recursion cap (encoding/json's decode limit is far higher).
const maxWalkDepth = 64

// denyURLHost inspects one string value: it returns a violation reason
// when the value carries a URL (any scheme, or the scheme-relative
// "//host" form, anywhere in the value) or a destination-shaped bare
// token bound for a denied host, and ok otherwise. Schemes are
// case-insensitive per RFC 3986. Allowlist entries win over every deny
// rule.
func denyURLHost(value string, allowedHosts []string) (reason string, denied bool) {
	// Every whitespace-separated token is a candidate, not just the first, so a
	// URL embedded in a command line ("curl http://169.254.169.254/") is
	// checked and a padded bare destination ("10.0.0.5 ", " localhost") is not
	// missed (NENYA-147).
	tokens := strings.Fields(value)
	for _, token := range tokens {
		trimmed := trimHostToken(token)
		if trimmed == "" {
			continue
		}
		switch {
		case urlSchemeRe.MatchString(trimmed):
			reason, denied, ok := denyAuthorityURL(trimmed, allowedHosts)
			if !ok {
				// A token that starts with a scheme is treated as a literal
				// URL: a malformed or host-less form fails closed (a lenient
				// downstream fetcher must not interpret what the policy could
				// not).
				return "URL is malformed and cannot be checked by policy", true
			}
			if denied {
				return reason, true
			}
		case strings.HasPrefix(trimmed, "//"):
			// Scheme-relative: judge the host as if it were https.
			reason, denied, ok := denyAuthorityURL("https:"+trimmed, allowedHosts)
			if !ok {
				return "URL is malformed and cannot be checked by policy", true
			}
			if denied {
				return reason, true
			}
		case embeddedURLRe.MatchString(trimmed):
			// A URL embedded in a larger token (a flag assignment, a markdown
			// link, a quoted string): evaluate the URL part itself. An
			// unparseable embedded URL is a template or a format string far more
			// often than a destination, so it falls through to the scheme-less
			// judgement instead of failing closed.
			if loc := embeddedURLRe.FindStringIndex(trimmed); loc != nil {
				if reason, denied, ok := denyAuthorityURL(trimURLTail(trimmed[loc[0]:]), allowedHosts); ok && denied {
					return reason, true
				}
			}
			if reason, denied := denyBareHostToken(trimmed, allowedHosts, len(tokens) == 1); denied {
				return reason, true
			}
		default:
			// A scheme-less token: a destination when it is a host-shaped
			// value (an IP literal, a host:port, a host/path, or a
			// special-use name).
			if reason, denied := denyBareHostToken(trimmed, allowedHosts, len(tokens) == 1); denied {
				return reason, true
			}
		}
	}
	return "", false
}

// denyAuthorityURL judges the authority of a URL-shaped candidate, reporting
// ok=false when the candidate is malformed or host-less (the caller decides
// whether that fails closed or falls back to the scheme-less judgement).
func denyAuthorityURL(candidate string, allowedHosts []string) (reason string, denied bool, ok bool) {
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Hostname() == "" {
		return "", false, false
	}
	host := netutil.NormalizeHost(parsed.Hostname())
	if hostAllowed(host, allowedHosts) {
		return "", false, true
	}
	if netutil.DenyHost(host) {
		return fmt.Sprintf("URL host %s is not allowed by policy", host), true, true
	}
	return "", false, true
}

// urlSchemeRe matches a token that starts with an RFC 3986 scheme and the "://"
// authority marker; a token merely *containing* "://" (a flag assignment, a
// quoted string, a regex) is not URL-shaped and is judged as a host instead.
var urlSchemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)

// embeddedURLRe finds a scheme-prefixed URL anywhere inside a token, so a URL
// glued to a flag, a markdown link, or a quoted string is still evaluated.
var embeddedURLRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://`)

// trimURLTail drops the delimiters a URL is commonly wrapped in.
func trimURLTail(raw string) string {
	return strings.TrimRight(raw, "\"'`<>()[]{},;")
}

// denyBareHostToken judges a scheme-less token: it strips an optional port and
// path/query, then denies a destination-shaped host. An IP literal (any family,
// including a bracketed IPv6), a host:port, a host/path, and the special-use
// suffixes (.local/.internal/.localhost) are destinations; the bare single-label
// "localhost" counts only when it is the whole value or carries a port/path, so
// prose that merely mentions the word is not rejected.
func denyBareHostToken(token string, allowedHosts []string, soleToken bool) (reason string, denied bool) {
	hostPart, port, hadPath := splitHostToken(token)
	host := netutil.NormalizeHost(hostPart)
	if host == "" {
		return "", false
	}
	if hostAllowed(host, allowedHosts) {
		return "", false
	}
	if ip, ok := netutil.ParseBareHost(host); ok {
		if netutil.DenyIP(ip) {
			return fmt.Sprintf("host %s is not allowed by policy", host), true
		}
		return "", false
	}
	// A special-use *name* is a destination only when it is destination-shaped
	// (the whole value, an explicit port, or a path): prose that merely mentions
	// "printer.local" or "db.internal" in a note must not reject the call
	// (NENYA-147). An IP literal above is always a destination.
	if isSpecialUseName(host) && (soleToken || port != "" || hadPath) {
		return fmt.Sprintf("host %s is not allowed by policy", host), true
	}
	return "", false
}

// isSpecialUseName reports whether a host name is a local/special-use name
// (localhost, or a .localhost/.internal/.local suffix).
func isSpecialUseName(host string) bool {
	return host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local")
}

// splitHostToken splits a scheme-less token into its host part and optional
// port, reporting whether a path/query/fragment followed. A bracketed IPv6
// literal keeps its address intact.
func splitHostToken(token string) (host, port string, hadPath bool) {
	host = token
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host, hadPath = host[:i], true
	}
	switch {
	case strings.HasPrefix(host, "["):
		if j := strings.Index(host, "]"); j >= 0 {
			return host[1:j], strings.TrimPrefix(host[j+1:], ":"), hadPath
		}
		return strings.Trim(host, "[]"), "", hadPath
	case strings.Count(host, ":") == 1:
		h, p, _ := strings.Cut(host, ":")
		return h, p, hadPath
	}
	return host, "", hadPath
}

// trimHostToken strips the punctuation an argument commonly glues to a URL or
// host (quotes, backticks, trailing separators). Brackets are preserved so a
// bracketed IPv6 literal survives to the host splitter.
func trimHostToken(token string) string {
	return strings.Trim(token, "\"'`<>(){},;")
}

// hostAllowed reports whether the host matches the allowlist: exact
// entries or "*.suffix" wildcards (suffix match on dot boundaries).
func hostAllowed(host string, allowedHosts []string) bool {
	for _, entry := range allowedHosts {
		entry = strings.ToLower(strings.TrimSpace(entry))
		switch {
		case entry == "":
			continue
		case strings.HasPrefix(entry, "*."):
			suffix := strings.TrimPrefix(entry, "*.")
			if host == suffix || strings.HasSuffix(host, "."+suffix) {
				return true
			}
		case entry == host:
			return true
		}
	}
	return false
}
