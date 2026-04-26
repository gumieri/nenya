// Package discovery implements dynamic model discovery by fetching /v1/models
// from each configured provider at startup and on SIGHUP reload. Discovered
// models are merged with the static ModelRegistry using a three-tier priority:
// config overrides > discovered models > static registry. It also manages
// provider health checks, capability detection, and pricing metadata.
package discovery
