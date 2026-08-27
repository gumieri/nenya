package config_test

import (
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/discovery"
)

// TestModelRegistry_CapabilitiesAreKnown ensures every Capabilities string in
// the static registry references a capability known to the discovery package.
// config cannot import internal/discovery (import cycle: discovery imports
// config), so the validation lives in this external test package.
//
// Note: no registry entry currently sets Capabilities (capability inference
// covers all built-in models), so this guard is forward-looking — it fires on
// the first entry that declares capabilities with a typo'd or unknown name.
func TestModelRegistry_CapabilitiesAreKnown(t *testing.T) {
	known := make(map[string]bool)
	for _, c := range discovery.AllCapabilities() {
		known[string(c)] = true
	}

	for modelID, entry := range config.ModelRegistry {
		for _, c := range entry.Capabilities {
			if c == "" {
				t.Errorf("model %q: empty capability string", modelID)
				continue
			}
			if !known[c] {
				t.Errorf("model %q: unknown capability %q", modelID, c)
			}
		}
	}
}
