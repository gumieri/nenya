package discovery

import (
	"testing"

	"github.com/nenya/config"
)

func aud67Config() *config.Config {
	return &config.Config{
		Providers: map[string]config.ProviderConfig{},
		Agents:    map[string]config.AgentConfig{},
	}
}

// TestMergeCatalog_DiscoveredBeatsStaticContext pins AUD-67's priority half:
// the documented three-tier priority (agent overrides > discovered > static)
// must hold in the static path too — pre-fix the primary entry preferred the
// STATIC max_context/max_output over the discovered values, contradicting the
// package doc, the override path's chain, and mergeWithStatic's own variant
// loop.
func TestMergeCatalog_DiscoveredBeatsStaticContext(t *testing.T) {
	const id = "aud67-context-priority"
	config.ModelRegistry[id] = config.ModelEntry{
		Provider:   "openai",
		MaxContext: 128000,
		MaxOutput:  4096,
	}
	t.Cleanup(func() { delete(config.ModelRegistry, id) })

	catalog := NewModelCatalog()
	catalog.Add(DiscoveredModel{
		ID: id, Provider: "openai", Format: "openai",
		MaxContext: 200000, MaxOutput: 8192, OwnedBy: "nenya",
	})

	merged := MergeCatalog(catalog, aud67Config())
	dm, ok := merged.Lookup(id)
	if !ok {
		t.Fatal("merged catalog missing model")
	}
	if dm.MaxContext != 200000 {
		t.Errorf("MaxContext = %d, want the discovered 200000 (documented priority)", dm.MaxContext)
	}
	if dm.MaxOutput != 8192 {
		t.Errorf("MaxOutput = %d, want the discovered 8192", dm.MaxOutput)
	}
}

// TestMergeCatalog_MetadataOverlayDoesNotMutateSourceCatalog pins the pointer
// half: overlaying static metadata must CLONE — pickMetadata used to hand out
// the discovery catalog's own *ModelMetadata and applyStaticEntryMetadata
// mutated it in place, corrupting the source catalog every later merge reads.
func TestMergeCatalog_MetadataOverlayDoesNotMutateSourceCatalog(t *testing.T) {
	const id = "aud67-metadata-clone"
	config.ModelRegistry[id] = config.ModelEntry{
		Provider:     "openai",
		ScoreBonus:   5,
		Capabilities: []string{"vision"},
	}
	t.Cleanup(func() { delete(config.ModelRegistry, id) })

	catalog := NewModelCatalog()
	catalog.Add(DiscoveredModel{
		ID: id, Provider: "openai", Format: "openai", MaxContext: 100000,
		Metadata: &ModelMetadata{ScoreBonus: 1},
	})

	merged := MergeCatalog(catalog, aud67Config())
	dm, ok := merged.Lookup(id)
	if !ok {
		t.Fatal("merged catalog missing model")
	}
	if dm.Metadata == nil || dm.Metadata.ScoreBonus != 5 {
		t.Fatalf("merged metadata must carry the static overlay: %+v", dm.Metadata)
	}
	src := catalog.LookupAll(id)
	if len(src) == 0 || src[0].Metadata == nil || src[0].Metadata.ScoreBonus != 1 {
		t.Fatalf("the SOURCE catalog's metadata must be untouched: %+v", src)
	}
}
