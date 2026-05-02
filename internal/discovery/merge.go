package discovery

import (
	"nenya/config"
)

// MergeCatalog rebuilds the merged model catalog from scratch on every call.
// This is intentional: the catalog is rebuilt only at startup and on SIGHUP reload,
// so the cost is negligible and correctness is simpler than incremental merging.
func MergeCatalog(catalog *ModelCatalog, cfg *config.Config) *ModelCatalog {
	merged := NewModelCatalog()
	agentOverrides := buildAgentOverrides(cfg)

	allModelIDs := make(map[string]bool)
	for id := range config.ModelRegistry {
		allModelIDs[id] = true
	}
	for _, m := range catalog.AllModels() {
		allModelIDs[m.ID] = true
	}

	for modelID := range allModelIDs {
		mergeModel(merged, modelID, catalog, agentOverrides)
	}
	return merged
}

func mergeModel(merged *ModelCatalog, modelID string, catalog *ModelCatalog, overrides map[string]agentOverride) {
	if override, hasOverride := overrides[modelID]; hasOverride {
		mergeWithOverride(merged, modelID, catalog, override)
		return
	}

	static, hasStatic := config.ModelRegistry[modelID]
	if hasStatic {
		mergeWithStatic(merged, modelID, catalog, static)
		return
	}

	if discovered, hasDiscovered := catalog.Lookup(modelID); hasDiscovered {
		merged.Add(discovered)
	}
}

func mergeWithOverride(merged *ModelCatalog, modelID string, catalog *ModelCatalog, override agentOverride) {
	static, hasStatic := config.ModelRegistry[modelID]
	discovered, hasDiscovered := catalog.Lookup(modelID)

	metadata := pickMetadata(discovered, hasDiscovered, static, hasStatic)

	merged.Add(DiscoveredModel{
		ID: modelID,
		Provider: firstNonEmpty(override.Provider,
			pickProvider(hasStatic, static.Provider, hasDiscovered, discovered.Provider)),
		Format: pickFormat(hasStatic, static.Format,
			hasDiscovered, discovered.Format),
		MaxContext: firstPositive(override.MaxContext,
			pickInt(hasDiscovered, discovered.MaxContext),
			pickInt(hasStatic, static.MaxContext)),
		MaxOutput: firstPositive(override.MaxOutput,
			pickInt(hasDiscovered, discovered.MaxOutput),
			pickInt(hasStatic, static.MaxOutput)),
		OwnedBy:  firstNonEmpty(discovered.OwnedBy, "nenya"),
		Metadata: metadata,
	})
}

func mergeWithStatic(merged *ModelCatalog, modelID string, catalog *ModelCatalog, static config.ModelEntry) {
	discovered, hasDiscovered := catalog.Lookup(modelID)

	metadata := pickMetadata(discovered, hasDiscovered, static, true)

	merged.Add(DiscoveredModel{
		ID: modelID,
		Provider: firstNonEmpty(static.Provider,
			pickProvider(false, "", hasDiscovered, discovered.Provider)),
		Format: pickFormat(true, static.Format,
			hasDiscovered, discovered.Format),
		MaxContext: firstPositive(static.MaxContext,
			pickInt(hasDiscovered, discovered.MaxContext)),
		MaxOutput: firstPositive(static.MaxOutput,
			pickInt(hasDiscovered, discovered.MaxOutput)),
		OwnedBy:  firstNonEmpty(discovered.OwnedBy, "nenya"),
		Metadata: metadata,
	})

	if hasDiscovered && discovered.Provider != "" && discovered.Provider != static.Provider {
		merged.Add(DiscoveredModel{
			ID:         modelID,
			Provider:   discovered.Provider,
			Format:     discovered.Format,
			MaxContext: firstPositive(discovered.MaxContext, static.MaxContext),
			MaxOutput:  firstPositive(discovered.MaxOutput, static.MaxOutput),
			OwnedBy:    firstNonEmpty(discovered.OwnedBy, "nenya"),
			Metadata:   metadata,
		})
	}
}

func pickMetadata(discovered DiscoveredModel, hasDiscovered bool, static config.ModelEntry, hasStatic bool) *ModelMetadata {
	var metadata *ModelMetadata
	if hasDiscovered && discovered.Metadata != nil {
		metadata = discovered.Metadata
	}

	if hasStatic && (static.ScoreBonus != 0 || len(static.Capabilities) > 0 || !static.Pricing.IsZero()) {
		if metadata == nil {
			metadata = &ModelMetadata{}
		}
		if static.ScoreBonus != 0 {
			metadata.ScoreBonus = static.ScoreBonus
		}
		metadata = applyCapabilities(metadata, static.Capabilities)
		if !static.Pricing.IsZero() {
			metadata.Pricing = &static.Pricing
		}
	}
	return metadata
}

type agentOverride struct {
	Provider   string
	MaxContext int
	MaxOutput  int
}

func buildAgentOverrides(cfg *config.Config) map[string]agentOverride {
	overrides := make(map[string]agentOverride)
	if cfg == nil || cfg.Agents == nil {
		return overrides
	}
	for _, agent := range cfg.Agents {
		for _, m := range agent.Models {
			if m.MaxContext > 0 || m.MaxOutput > 0 || m.Provider != "" {
				o := overrides[m.Model]
				if m.Provider != "" {
					o.Provider = m.Provider
				}
				if m.MaxContext > 0 {
					o.MaxContext = m.MaxContext
				}
				if m.MaxOutput > 0 {
					o.MaxOutput = m.MaxOutput
				}
				overrides[m.Model] = o
			}
		}
	}
	return overrides
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstPositive(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

func pickProvider(staticExists bool, staticVal string, discExists bool, discVal string) string {
	if staticExists && staticVal != "" {
		return staticVal
	}
	if discExists && discVal != "" {
		return discVal
	}
	return ""
}

func pickFormat(staticExists bool, staticVal string, discExists bool, discVal string) string {
	if staticExists && staticVal != "" {
		return staticVal
	}
	if discExists && discVal != "" {
		return discVal
	}
	return ""
}

func pickInt(exists bool, val int) int {
	if exists {
		return val
	}
	return 0
}
