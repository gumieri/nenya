package config

// Diagnostic is a non-fatal issue found while resolving configuration or
// secrets, reported by `nenya describe --json` (CONTRACT.md §4.3). It never
// prevents startup.
type Diagnostic struct {
	// Level is "warn" or "error" today; "info" is reserved for future
	// informational diagnostics.
	Level string `json:"level"`
	// Code is a stable machine identifier (for example "unknown_field",
	// "config_not_found", "config_path_is_directory", "secrets_not_found",
	// "secrets_invalid").
	Code string `json:"code"`
	// Message is a human-readable description.
	Message string `json:"message"`
	// Source names the file or path the issue concerns, when known.
	Source string `json:"source,omitempty"`
}
