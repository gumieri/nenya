// Package netutil provides shared outbound-transport network policy
// helpers: the explicit TLS version floor, private CA bundle loading,
// egress proxy resolution (explicit URL with HTTPS_PROXY/NO_PROXY
// environment fallback), and the destination-deny helpers shared by the
// untrusted-content URL policies (ExfilGuard and the MCP tool-argument
// guard) — host normalization, IP-literal recognition in every accepted
// spelling, and the private/loopback/link-local/CGNAT/NAT64 deny set
// (NENYA-137, NENYA-147). Stdlib-only leaf: imported by config validation
// and every transport construction site. It must not import any other
// repository package.
package netutil
