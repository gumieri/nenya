// Package gateway defines the NenyaGateway struct, which is the top-level
// container wiring together configuration, HTTP clients, provider registries,
// MCP clients, metrics, rate limiting, caching, and the token counter. The
// gateway is created once at startup and atomically swapped on config reload.
package gateway
