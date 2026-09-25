package mcp

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// checkURLs walks the argument tree and flags http(s) URLs hosted on
// denied destinations (private/loopback/link-local, localhost variants,
// .internal/.local suffixes) unless the host is allowlisted. Policy
// "off" returns nothing; "log" still collects flags so the caller can
// record them, but rejection is decided by ValidateArgs.
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
// when the value is an http(s) URL bound for a denied host, and ok
// otherwise (non-URL strings are not the argument guard's business).
// Schemes are case-insensitive per RFC 3986. Allowlist entries win over
// every deny rule.
func denyURLHost(value string, allowedHosts []string) (reason string, denied bool) {
	// Classify on the leading whitespace-delimited token: a string
	// field may carry prose after a URL ("https://example.com is the
	// docs"), and parsing the whole value would misclassify it as
	// malformed. The first token is what a fetcher would attempt.
	token := value
	if fields := strings.Fields(value); len(fields) > 0 {
		token = fields[0]
	}
	lowered := strings.ToLower(token)
	if !strings.HasPrefix(lowered, "http://") && !strings.HasPrefix(lowered, "https://") {
		return "", false
	}
	parsed, err := url.Parse(token)
	if err != nil || parsed.Hostname() == "" {
		// URL-shaped but unparseable or host-less (opaque forms,
		// embedded backslashes): fail closed rather than let a lenient
		// downstream fetcher interpret it.
		return "URL is malformed and cannot be checked by policy", true
	}
	// The prefix gate guarantees the http(s) scheme here.
	host := strings.ToLower(parsed.Hostname())
	host = strings.TrimRight(host, ".")
	if before, _, found := strings.Cut(host, "%"); found {
		// IPv6 zone identifier ([fe80::1%25eth0]): the zone is local
		// scoping, the address part decides the policy.
		host = before
	}
	if hostAllowed(host, allowedHosts) {
		return "", false
	}
	if hostDenied(host) {
		return fmt.Sprintf("URL host %s is not allowed by policy", host), true
	}
	return "", false
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

// hostDenied applies the built-in deny rules: localhost variants and
// .internal/.local special-use suffixes, IP literals in every accepted
// spelling (dotted, decimal/hex/octal integer forms — glibc-style
// normalization —, both IPv6 families, v4-mapped and NAT64-embedded),
// and the deny IP set below.
func hostDenied(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return denyIP(ip)
	}
	// net.ParseIP rejects legacy integer IPv4 spellings that OS
	// resolvers still accept (2130706433, 0x7f000001, 0177.0.0.1,
	// 127.1): normalize before failing open.
	if ip, ok := parseNumericIPv4(host); ok {
		return denyIP(ip)
	}
	return false
}

// parseNumericIPv4 normalizes integer-form IPv4 literals: 1 to 4
// dot-separated labels, each decimal, 0x-hex, or 0-octal, composed
// big-endian per classic inet_aton partial-address rules (the last
// label fills all remaining bytes, earlier labels are single bytes).
func parseNumericIPv4(host string) (net.IP, bool) {
	labels := strings.Split(host, ".")
	if len(labels) > 4 {
		return nil, false
	}
	var parts [4]byte
	for i, label := range labels {
		value, ok := parseHostLabel(label)
		if !ok {
			return nil, false
		}
		if i < len(labels)-1 {
			// Non-final labels are single bytes.
			if value > 0xff {
				return nil, false
			}
			parts[i] = byte(value)
			continue
		}
		// The final label covers all remaining bytes.
		remaining := 4 - i
		maxValue := uint64(1)<<(8*remaining) - 1
		if value > maxValue {
			return nil, false
		}
		for shift := remaining - 1; shift >= 0; shift-- {
			parts[i+shift] = byte(value >> (8 * (remaining - 1 - shift)))
		}
	}
	return net.IPv4(parts[0], parts[1], parts[2], parts[3]), true
}

// parseHostLabel parses one IPv4-literal label in decimal, 0x-hex, or
// 0-octal form.
func parseHostLabel(label string) (uint64, bool) {
	if label == "" {
		return 0, false
	}
	base := 10
	digits := label
	switch {
	case strings.HasPrefix(label, "0x") || strings.HasPrefix(label, "0X"):
		base, digits = 16, label[2:]
	case label[0] == '0' && len(label) > 1:
		base, digits = 8, label[1:]
	}
	if digits == "" {
		return 0, false
	}
	parsed, err := strconv.ParseUint(digits, base, 32)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// denyIP applies the destination deny set to a parsed address of
// either family.
func denyIP(ip net.IP) bool {
	if v4 := mappedIPv4(ip); v4 != nil {
		return denyIPv4(v4)
	}
	return denyIPv6(ip)
}

// mappedIPv4 returns the embedded IPv4 address for 4-byte inputs and
// the ::ffff:a.b.c.d mapping form. Legacy ::/96-compatible and NAT64
// embeddings stay with denyIPv6, which must first judge the address by
// its IPv6 predicates (e.g. ::1 is IPv6 loopback, not 0.0.0.1).
func mappedIPv4(ip net.IP) net.IP {
	if len(ip) == net.IPv4len {
		return ip
	}
	if len(ip) == net.IPv6len && bytesZero(ip[:10]) && ip[10] == 0xff && ip[11] == 0xff {
		return ip[12:16]
	}
	return nil
}

func bytesZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func denyIPv4(ip net.IP) bool {
	// Loopback 127/8, private RFC1918, link-local 169.254/16 (includes
	// cloud metadata), "this network" 0.0.0.0/8, broadcast
	// 255.255.255.255, multicast, CGNAT 100.64/10.
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) {
		return true
	}
	if net.IPv4(0, 0, 0, 0).Mask(net.CIDRMask(8, 32)).Equal(ip.Mask(net.CIDRMask(8, 32))) {
		return true
	}
	cgnat := net.IPv4(100, 64, 0, 0)
	return cgnat.Mask(net.CIDRMask(10, 32)).Equal(ip.Mask(net.CIDRMask(10, 32)))
}

func denyIPv6(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsPrivate() || ip.IsMulticast() {
		return true
	}
	// Embedded IPv4 forms (::/96 legacy-compatible, 64:ff9b::/96 NAT64)
	// are judged by their embedded v4 destination.
	if embedded, ok := embeddedIPv4(ip); ok {
		return denyIPv4(embedded)
	}
	return false
}

// embeddedIPv4 extracts the trailing IPv4 address from the
// /96-prefixed embedding forms (legacy ::/96-compatible and
// 64:ff9b::/96 NAT64); ok=false for all other addresses. The IPv4-
// translated ::ffff:0:0/96 form (RFC 2765) is not extracted: Linux
// does not route it to the embedded address, so it is not a live
// bypass — but the deny set does not claim to cover it.
func embeddedIPv4(ip net.IP) (net.IP, bool) {
	if len(ip) != net.IPv6len {
		return nil, false
	}
	prefixAllZero := true
	for _, b := range ip[:12] {
		if b != 0 {
			prefixAllZero = false
			break
		}
	}
	if prefixAllZero {
		return net.IP(append(net.IP{}, ip[12:]...)), true
	}
	nat64 := ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b
	if nat64 {
		for _, b := range ip[4:12] {
			if b != 0 {
				nat64 = false
				break
			}
		}
	}
	if nat64 {
		return net.IP(append(net.IP{}, ip[12:]...)), true
	}
	return nil, false
}
