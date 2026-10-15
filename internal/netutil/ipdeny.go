package netutil

import (
	"net"
	"strconv"
	"strings"
)

// This file holds the shared destination-deny helpers for host values taken
// from untrusted content (tool arguments, model-produced URLs). Both the MCP
// argument guard and the ExfilGuard URL policy must agree on what a "private
// destination" is, and both must normalize the legacy integer IPv4 spellings
// that OS resolvers still accept (NENYA-147). Stdlib-only, no repository
// imports.

// DenyHost applies the built-in deny rules to a host name: localhost variants,
// the .internal/.local special-use suffixes, and IP literals in every accepted
// spelling (dotted, decimal/hex/octal integer forms, both IPv6 families,
// v4-mapped and NAT64-embedded). Callers must pass a lowercased host with any
// trailing dot and IPv6 zone removed.
func DenyHost(host string) bool {
	if host == "" {
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return DenyIP(ip)
	}
	// net.ParseIP rejects legacy integer IPv4 spellings that OS resolvers
	// still accept (2130706433, 0x7f000001, 0177.0.0.1, 127.1): normalize
	// before failing open.
	if ip, ok := ParseNumericIPv4(host); ok {
		return DenyIP(ip)
	}
	return false
}

// IsIPLiteral reports whether a host is an IP literal in any accepted spelling
// (including the legacy integer IPv4 forms), returning the parsed address.
func IsIPLiteral(host string) (net.IP, bool) {
	if ip := net.ParseIP(host); ip != nil {
		return ip, true
	}
	return ParseNumericIPv4(host)
}

// NormalizeHost lowercases a host, removes every trailing dot, and cuts an
// IPv6 zone suffix (fe80::1%eth0). It is the canonical preparation for
// DenyHost and for allowlist comparison. The zone cut applies only to an IPv6
// literal (a host containing a colon), so a percent sign in a DNS name is not
// silently truncated.
func NormalizeHost(host string) string {
	host = strings.ToLower(host)
	host = strings.TrimRight(host, ".")
	if strings.Contains(host, ":") {
		if before, _, found := strings.Cut(host, "%"); found {
			host = before
		}
	}
	return host
}

// ParseBareHost parses a scheme-less host token as an address: a full IP in
// either family (net.ParseIP, including a bracketed or bare IPv6 literal) or a
// standalone-plausible integer IPv4 spelling.
func ParseBareHost(host string) (net.IP, bool) {
	if ip := net.ParseIP(host); ip != nil {
		return ip, true
	}
	return ParseStandaloneIPv4(host)
}

// ParseStandaloneIPv4 parses a *standalone* value as an IPv4 literal, accepting
// only the spellings that are plausible as a host: a complete dotted IPv4 (four
// labels) or an explicit 0x hex integer. A partial value ("0.5", "10.0",
// "10.0.0") is a version, a range bound, or a decimal far more often than a
// 32-bit address, so treating it as one would reject ordinary tool arguments
// (NENYA-147). URL hosts use ParseNumericIPv4 instead, where the browser-style
// interpretation is the right one.
func ParseStandaloneIPv4(host string) (net.IP, bool) {
	// A complete dotted IPv4 (four labels) or an explicit 0x hex integer. A
	// partial value ("0.5", "10.0", "10.0.0", "127.0") is a version, a range
	// bound, or a decimal far more often than an address; a bare decimal
	// ("2130706433") or a leading-zero mode ("0644") is not an address when it
	// stands alone.
	if !strings.HasPrefix(host, "0x") && !strings.HasPrefix(host, "0X") && strings.Count(host, ".") < 3 {
		return nil, false
	}
	return ParseNumericIPv4(host)
}

// ParseNumericIPv4 normalizes integer-form IPv4 literals: 1 to 4
// dot-separated labels, each decimal, 0x-hex, or 0-octal, composed
// big-endian per classic inet_aton partial-address rules (the last
// label fills all remaining bytes, earlier labels are single bytes).
func ParseNumericIPv4(host string) (net.IP, bool) {
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

// DenyIP applies the destination deny set to a parsed address of
// either family.
func DenyIP(ip net.IP) bool {
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
	prefixAllZero := bytesZero(ip[:12])
	if prefixAllZero {
		return net.IP(append(net.IP{}, ip[12:]...)), true
	}
	nat64 := ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b && bytesZero(ip[4:12])
	if nat64 {
		return net.IP(append(net.IP{}, ip[12:]...)), true
	}
	return nil, false
}
