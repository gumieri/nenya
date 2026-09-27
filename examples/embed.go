// Package examples provides the canonical example configuration shipped with
// Nenya so the binary can print it (nenya example-config) from the same file
// that is packaged (CONTRACT.md §4.4).
package examples

import _ "embed"

// ConfigJSON is the canonical example configuration in JSONC form.
//
//go:embed example.config.json
var ConfigJSON []byte
