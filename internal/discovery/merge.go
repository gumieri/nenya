package discovery

import (
	"nenya/internal/config"
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
		static, hasStatic := config.ModelRegistry[modelID]
		discovered, hasDiscovered := catalog.Lookup(modelID)
		override, hasOverride := agentOverrides[modelID]

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

		if hasOverride {
			merged.Add(DiscoveredModel{
				ID:         modelID,
				Provider:   firstNonEmpty(override.Provider, pickProvider(hasStatic, static.Provider, hasDiscovered, discovered.Provider)),
				MaxContext: firstPositive(override.MaxContext, pickInt(hasDiscovered, discovered.MaxContext), pickInt(hasStatic, static.MaxContext)),
				MaxOutput:  firstPositive(override.MaxOutput, pickInt(hasDiscovered, discovered.MaxOutput), pickInt(hasStatic, static.MaxOutput)),
				OwnedBy:    firstNonEmpty(discovered.OwnedBy, "nenya"),
				Metadata:   metadata,
			})
		} else if hasStatic {
			merged.Add(DiscoveredModel{
				ID:         modelID,
				Provider:   firstNonEmpty(static.Provider, pickProvider(false, "", hasDiscovered, discovered.Provider)),
				MaxContext: firstPositive(static.MaxContext, pickInt(hasDiscovered, discovered.MaxContext)),
				MaxOutput:  firstPositive(static.MaxOutput, pickInt(hasDiscovered, discovered.MaxOutput)),
				OwnedBy:    firstNonEmpty(discovered.OwnedBy, "nenya"),
				Metadata:   metadata,
			})
		} else if hasDiscovered {
			merged.Add(discovered)
		}
	}

	return merged
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
				o, exists := overrides[m.Model]
				if !exists {
					o = agentOverride{}
				}
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

func pickInt(exists bool, val int) int {
	if exists {
		return val
	}
	return 0
}
