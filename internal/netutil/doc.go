// Package netutil provides shared outbound-transport network policy
// helpers: the explicit TLS version floor, private CA bundle loading, and
// egress proxy resolution (explicit URL with HTTPS_PROXY/NO_PROXY
// environment fallback). Stdlib-only leaf: imported by config validation
// and every transport construction site (NENYA-137). It must not import
// any other repository package.
package netutil
