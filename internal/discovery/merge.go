// Package discovery merges static model registry with dynamically discovered
// models from provider /v1/models endpoints.
//
// Merge priority (highest to lowest):
// 1. Agent model overrides (per-model max_context/max_output/provider)
// 2. Dynamically discovered models (from provider catalogs)
// 3. Static model registry (built-in defaults)
//
// The merged catalog is used throughout Nenya for:
// - Model resolution in routing (resolveProvider)
// - /v1/models endpoint responses
// - max_tokens injection by provider capabilities
//
// Merge is performed at startup and on SIGHUP reload.
package discovery

import (
	"regexp"

	"github.com/nenya/config"
)

// MergeCatalog rebuilds the merged model catalog from scratch on every call.
// This is intentional: the catalog is rebuilt only at startup and on SIGHUP reload,
// so the cost is negligible and correctness is simpler than incremental merging.
func MergeCatalog(catalog *ModelCatalog, cfg *config.Config) *ModelCatalog {
	merged := NewModelCatalog()
	agentOverrides := buildAgentOverrides(cfg)
	providerAllows := buildProviderAllows(cfg.Providers)

	allModelIDs := make(map[string]bool)
	for id := range config.ModelRegistry {
		allModelIDs[id] = true
	}
	for _, m := range catalog.AllModels() {
		allModelIDs[m.ID] = true
	}

	for modelID := range allModelIDs {
		mergeModel(merged, modelID, catalog, agentOverrides, providerAllows)
	}
	return merged
}

// buildProviderAllows compiles provider allowed_models patterns for catalog merge.
// Returns map[providerName][]*regexp.Regexp (nil = no filtering, empty slice = allow all).
func buildProviderAllows(providers map[string]config.ProviderConfig) map[string][]*regexp.Regexp {
	if len(providers) == 0 {
		return nil
	}
	result := make(map[string][]*regexp.Regexp, len(providers))
	for name, pc := range providers {
		if len(pc.AllowedModels) == 0 {
			continue
		}
		res := make([]*regexp.Regexp, 0, len(pc.AllowedModels))
		for _, pat := range pc.AllowedModels {
			re, err := regexp.Compile(pat)
			if err != nil {
				continue
			}
			res = append(res, re)
		}
		if len(res) > 0 {
			result[name] = res
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func isModelAllowed(providerAllows map[string][]*regexp.Regexp, provider, model string) bool {
	if len(providerAllows) == 0 {
		return true
	}
	patterns, has := providerAllows[provider]
	if !has || len(patterns) == 0 {
		return true
	}
	for _, re := range patterns {
		if re.MatchString(model) {
			return true
		}
	}
	return false
}

func mergeModel(merged *ModelCatalog, modelID string, catalog *ModelCatalog, overrides map[string]agentOverride, providerAllows map[string][]*regexp.Regexp) {
	if override, hasOverride := overrides[modelID]; hasOverride {
		mergeWithOverride(merged, modelID, catalog, override, providerAllows)
		return
	}

	static, hasStatic := config.ModelRegistry[modelID]
	if hasStatic {
		mergeWithStatic(merged, modelID, catalog, static, providerAllows)
		return
	}

	entries := catalog.LookupAll(modelID)
	seenProviders := map[string]bool{}
	for _, dm := range entries {
		if !seenProviders[dm.Provider] && isModelAllowed(providerAllows, dm.Provider, modelID) {
			seenProviders[dm.Provider] = true
			merged.Add(dm)
		}
	}
}

func mergeWithOverride(merged *ModelCatalog, modelID string, catalog *ModelCatalog, override agentOverride, providerAllows map[string][]*regexp.Regexp) {
	static, hasStatic := config.ModelRegistry[modelID]
	allDiscovered := catalog.LookupAll(modelID)

	var discovered DiscoveredModel
	hasDiscovered := len(allDiscovered) > 0
	if hasDiscovered {
		discovered = allDiscovered[0]
	}

	metadata := pickMetadata(discovered, hasDiscovered, static, hasStatic)

	primaryProvider := firstNonEmpty(override.Provider,
		pickProvider(hasStatic, static.Provider, hasDiscovered, discovered.Provider))

	if !isModelAllowed(providerAllows, primaryProvider, modelID) {
		return
	}

	merged.Add(DiscoveredModel{
		ID:       modelID,
		Provider: primaryProvider,
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

	seenProviders := map[string]bool{primaryProvider: true}
	for _, dm := range allDiscovered {
		if dm.Provider != "" && !seenProviders[dm.Provider] && isModelAllowed(providerAllows, dm.Provider, modelID) {
			seenProviders[dm.Provider] = true
			merged.Add(DiscoveredModel{
				ID:       modelID,
				Provider: dm.Provider,
				Format:   dm.Format,
				MaxContext: firstPositive(dm.MaxContext,
					pickInt(hasDiscovered, discovered.MaxContext),
					pickInt(hasStatic, static.MaxContext)),
				MaxOutput: firstPositive(dm.MaxOutput,
					pickInt(hasDiscovered, discovered.MaxOutput),
					pickInt(hasStatic, static.MaxOutput)),
				OwnedBy:  firstNonEmpty(dm.OwnedBy, "nenya"),
				Metadata: metadata,
			})
		}
	}
}

func mergeWithStatic(merged *ModelCatalog, modelID string, catalog *ModelCatalog, static config.ModelEntry, providerAllows map[string][]*regexp.Regexp) {
	allDiscovered := catalog.LookupAll(modelID)

	// Use the first discovered entry for metadata and format fallback.
	// All provider entries for a model share the same metadata (merged from
	// discovered + static capabilities/pricing) since capabilities and pricing
	// are model-level attributes, not provider-level.
	var discovered DiscoveredModel
	hasDiscovered := len(allDiscovered) > 0
	if hasDiscovered {
		discovered = allDiscovered[0]
	}

	metadata := pickMetadata(discovered, hasDiscovered, static, true)

	primaryProvider := firstNonEmpty(static.Provider,
		pickProvider(false, "", hasDiscovered, discovered.Provider))

	if !isModelAllowed(providerAllows, primaryProvider, modelID) {
		return
	}

	merged.Add(DiscoveredModel{
		ID:       modelID,
		Provider: primaryProvider,
		Format: pickFormat(true, static.Format,
			hasDiscovered, discovered.Format),
		MaxContext: firstPositive(static.MaxContext,
			pickInt(hasDiscovered, discovered.MaxContext)),
		MaxOutput: firstPositive(static.MaxOutput,
			pickInt(hasDiscovered, discovered.MaxOutput)),
		OwnedBy:  firstNonEmpty(discovered.OwnedBy, "nenya"),
		Metadata: metadata,
	})

	seenProviders := map[string]bool{primaryProvider: true}
	for _, dm := range allDiscovered {
		if dm.Provider != "" && !seenProviders[dm.Provider] && isModelAllowed(providerAllows, dm.Provider, modelID) {
			seenProviders[dm.Provider] = true
			merged.Add(DiscoveredModel{
				ID:       modelID,
				Provider: dm.Provider,
				Format:   dm.Format,
				MaxContext: firstPositive(dm.MaxContext,
					pickInt(hasDiscovered, discovered.MaxContext),
					static.MaxContext),
				MaxOutput: firstPositive(dm.MaxOutput,
					pickInt(hasDiscovered, discovered.MaxOutput),
					static.MaxOutput),
				OwnedBy:  firstNonEmpty(dm.OwnedBy, "nenya"),
				Metadata: metadata,
			})
		}
	}
}

func pickMetadata(discovered DiscoveredModel, hasDiscovered bool, static config.ModelEntry, hasStatic bool) *ModelMetadata {
	var metadata *ModelMetadata
	if hasDiscovered && discovered.Metadata != nil {
		metadata = discovered.Metadata
	}

	if !hasStatic || (static.ScoreBonus == 0 && len(static.Capabilities) == 0 && static.Pricing.IsZero()) {
		return metadata
	}
	return applyStaticEntryMetadata(metadata, static)
}

// applyStaticEntryMetadata overlays a static registry entry's metadata (score
// bonus, explicit capabilities, pricing) onto meta. Merge semantics are
// additive: config-supplied capabilities can add to inferred capabilities but
// never remove them.
func applyStaticEntryMetadata(meta *ModelMetadata, entry config.ModelEntry) *ModelMetadata {
	if meta == nil {
		meta = &ModelMetadata{}
	}
	if entry.ScoreBonus != 0 {
		meta.ScoreBonus = entry.ScoreBonus
	}
	if len(entry.Capabilities) > 0 {
		caps := make([]Capability, len(entry.Capabilities))
		for i, c := range entry.Capabilities {
			caps[i] = Capability(c)
		}
		meta = applyCapabilities(meta, caps)
	}
	if !entry.Pricing.IsZero() {
		// Copy the value: all models referencing this static entry would
		// otherwise share one *PricingOverride pointing into the global
		// config registry, so mutating any catalog model's pricing would
		// corrupt the shared registry entry.
		p := entry.Pricing
		meta.Pricing = &p
	}
	return meta
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
