package routing

import (
	"testing"

	"github.com/nenya/config"
)

// withRegistryEntry adds a ModelRegistry entry for the test and restores the
// previous state on cleanup (the registry is a package-level global).
func withRegistryEntry(t *testing.T, id string, entry config.ModelEntry) {
	t.Helper()
	prev, had := config.ModelRegistry[id]
	config.ModelRegistry[id] = entry
	t.Cleanup(func() {
		if had {
			config.ModelRegistry[id] = prev
			return
		}
		delete(config.ModelRegistry, id)
	})
}

// TestResolveEffectiveLimitsFoldRegistry pins the transform-side fallbacks
// (max-output clamp and context budget): both must fold plain-ID spellings
// against the static registry when the catalog has no entry.
func TestResolveEffectiveLimitsFoldRegistry(t *testing.T) {
	withRegistryEntry(t, "claude-3-5-sonnet", config.ModelEntry{MaxContext: 200000, MaxOutput: 8192})

	deps := TransformDeps{} // no catalog: the registry fallback decides
	if got := resolveEffectiveMaxOutput(deps, "Claude-3-5-Sonnet", 0); got != 8192 {
		t.Fatalf("resolveEffectiveMaxOutput(Claude-3-5-Sonnet) = %d, want 8192", got)
	}
	if got := resolveEffectiveMaxContext(deps, "CLAUDE-3-5-SONNET"); got != 200000 {
		t.Fatalf("resolveEffectiveMaxContext(CLAUDE-3-5-SONNET) = %d, want 200000", got)
	}

	// Opaque exactness carries over here too.
	withRegistryEntry(t, "dotted.Model-ID", config.ModelEntry{MaxContext: 32000, MaxOutput: 4096})
	if got := resolveEffectiveMaxContext(deps, "DOTTED.model-id"); got != 0 {
		t.Fatalf("a re-cased opaque ID must not match, got ctx=%d", got)
	}
	if got := resolveEffectiveMaxContext(deps, "dotted.Model-ID"); got != 32000 {
		t.Fatalf("exact opaque spelling must resolve, got ctx=%d", got)
	}
}
