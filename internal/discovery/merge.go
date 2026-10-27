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
	"sort"

	"github.com/nenya/config"
)

// NonChatDrops reports the models a catalog operation dropped because a
// provider's non_chat_models classified them as decision-only: provider name ->
// model IDs (de-duplicated, sorted). The gateway logs them and turns them into
// the nenya_non_chat_models_dropped_total counter (NENYA-180), so the
// classification is observable instead of silent. A nil report means nothing was
// dropped.
//
// Scope: the merge reports what it filters out of the registry/catalog input it
// was given, so an agent-only model ID (which the merge never sees) is not
// reported; the fetch-time filter reports its own drops separately.
type NonChatDrops map[string][]string

// MergeCatalog rebuilds the merged model catalog from scratch on every call.
// This is intentional: the catalog is rebuilt only at startup and on SIGHUP reload,
// so the cost is negligible and correctness is simpler than incremental merging.
func MergeCatalog(catalog *ModelCatalog, cfg *config.Config) *ModelCatalog {
	merged, _ := MergeCatalogWithDrops(catalog, cfg)
	return merged
}

// MergeCatalogWithDrops is MergeCatalog plus the non_chat_models drop report.
// The report is returned separately so the common call site keeps its
// single-value shape.
func MergeCatalogWithDrops(catalog *ModelCatalog, cfg *config.Config) (*ModelCatalog, NonChatDrops) {
	merged := NewModelCatalog()
	drops := NonChatDrops{}
	agentOverrides := buildAgentOverrides(cfg)
	providerAllows := buildProviderAllows(cfg.Providers)
	nonChat := newNonChatFilter(cfg.Providers, drops)

	allModelIDs := make(map[string]bool)
	for id := range config.ModelRegistry {
		allModelIDs[id] = true
	}
	for _, m := range catalog.AllModels() {
		allModelIDs[m.ID] = true
	}

	for modelID := range allModelIDs {
		mergeModel(merged, modelID, catalog, agentOverrides, providerAllows, &nonChat)
	}
	if len(drops) == 0 {
		return merged, nil
	}
	return merged, drops
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

// buildProviderNonChat compiles non_chat_models patterns per provider.
// Returns map[providerName][]*regexp.Regexp (nil = no filtering).
func buildProviderNonChat(providers map[string]config.ProviderConfig) map[string][]*regexp.Regexp {
	if len(providers) == 0 {
		return nil
	}
	result := make(map[string][]*regexp.Regexp, len(providers))
	for name, pc := range providers {
		if len(pc.NonChatModels) == 0 {
			continue
		}
		res := make([]*regexp.Regexp, 0, len(pc.NonChatModels))
		for _, pat := range pc.NonChatModels {
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

// nonChatFilter carries the compiled per-provider non_chat_models patterns plus
// an optional drop recorder, so one predicate both filters and reports what it
// dropped (NENYA-180). A nil recorder makes it a pure predicate.
type nonChatFilter struct {
	patterns map[string][]*regexp.Regexp
	drops    NonChatDrops
}

// newNonChatFilter compiles the per-provider patterns and attaches a recorder.
func newNonChatFilter(providers map[string]config.ProviderConfig, drops NonChatDrops) nonChatFilter {
	return nonChatFilter{patterns: buildProviderNonChat(providers), drops: drops}
}

// matches reports whether the (provider, model) pair is classified non-chat,
// recording the drop when a recorder is attached. It is a predicate with a
// side effect on purpose: every call site is a filter decision, and the
// short-circuit order means a pair dropped by allowed_models is never recorded
// as a non-chat drop.
func (f *nonChatFilter) matches(provider, model string) bool {
	if !isNonChat(f.patterns, provider, model) {
		return false
	}
	f.record(provider, model)
	return true
}

// record adds one (provider, model) drop to the report, de-duplicated and kept
// sorted so the report (and the log line built from it) is deterministic.
func (f *nonChatFilter) record(provider, model string) {
	if f.drops == nil {
		return
	}
	for _, existing := range f.drops[provider] {
		if existing == model {
			return
		}
	}
	models := append(f.drops[provider], model)
	sort.Strings(models)
	f.drops[provider] = models
}

// isNonChat reports whether the (provider, model) pair is classified as a
// non-chat model by providerNonChat patterns.
func isNonChat(providerNonChat map[string][]*regexp.Regexp, provider, model string) bool {
	if len(providerNonChat) == 0 {
		return false
	}
	patterns, has := providerNonChat[provider]
	if !has || len(patterns) == 0 {
		return false
	}
	for _, re := range patterns {
		if re.MatchString(model) {
			return true
		}
	}
	return false
}

// ApplyProviderNonChatToCatalog removes non-chat models from a catalog that
// was built without the non_chat_models filter, mutating it in place. Used
// for personalities that consume an externally-built catalog (e.g. the
// pricing fetcher) where the raw provider config is the source of truth.
func ApplyProviderNonChatToCatalog(catalog *ModelCatalog, providers map[string]config.ProviderConfig) NonChatDrops {
	if catalog == nil || len(providers) == 0 {
		return nil
	}
	drops := NonChatDrops{}
	nonChat := newNonChatFilter(providers, drops)
	if len(nonChat.patterns) == 0 {
		return nil
	}
	catalog.Remove(func(m DiscoveredModel) bool {
		return nonChat.matches(m.Provider, m.ID)
	})
	if len(drops) == 0 {
		return nil
	}
	return drops
}

func mergeModel(merged *ModelCatalog, modelID string, catalog *ModelCatalog, overrides map[string]agentOverride, providerAllows map[string][]*regexp.Regexp, nonChat *nonChatFilter) {
	if override, hasOverride := overrides[modelID]; hasOverride {
		mergeWithOverride(merged, modelID, catalog, override, providerAllows, nonChat)
		return
	}

	static, hasStatic := config.ModelRegistry[modelID]
	if hasStatic {
		mergeWithStatic(merged, modelID, catalog, static, providerAllows, nonChat)
		return
	}

	entries := catalog.LookupAll(modelID)
	seenProviders := map[string]bool{}
	for _, dm := range entries {
		if !seenProviders[dm.Provider] && isModelAllowed(providerAllows, dm.Provider, modelID) && !nonChat.matches(dm.Provider, modelID) {
			seenProviders[dm.Provider] = true
			merged.Add(dm)
		}
	}
}

func mergeWithOverride(merged *ModelCatalog, modelID string, catalog *ModelCatalog, override agentOverride, providerAllows map[string][]*regexp.Regexp, nonChat *nonChatFilter) {
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

	if !isModelAllowed(providerAllows, primaryProvider, modelID) || nonChat.matches(primaryProvider, modelID) {
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
		Pricing:  pricingEntryFromOverride(static.Pricing),
	})

	seenProviders := map[string]bool{primaryProvider: true}
	for _, dm := range allDiscovered {
		if dm.Provider != "" && !seenProviders[dm.Provider] && isModelAllowed(providerAllows, dm.Provider, modelID) && !nonChat.matches(dm.Provider, modelID) {
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
				Pricing:  pricingEntryFromOverride(static.Pricing),
			})
		}
	}
}

func mergeWithStatic(merged *ModelCatalog, modelID string, catalog *ModelCatalog, static config.ModelEntry, providerAllows map[string][]*regexp.Regexp, nonChat *nonChatFilter) {
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

	if !isModelAllowed(providerAllows, primaryProvider, modelID) || nonChat.matches(primaryProvider, modelID) {
		return
	}

	merged.Add(DiscoveredModel{
		ID:       modelID,
		Provider: primaryProvider,
		Format: pickFormat(true, static.Format,
			hasDiscovered, discovered.Format),
		MaxContext: firstPositive(pickInt(hasDiscovered, discovered.MaxContext),
			static.MaxContext),
		MaxOutput: firstPositive(pickInt(hasDiscovered, discovered.MaxOutput),
			static.MaxOutput),
		OwnedBy:  firstNonEmpty(discovered.OwnedBy, "nenya"),
		Metadata: metadata,
		// Static registry pricing becomes the catalog pricing entry so
		// billing and the cost guard see it (docs/COST_MODEL.md); an
		// attached external pricing feed may later override the baseline.
		Pricing: pricingEntryFromOverride(static.Pricing),
	})

	seenProviders := map[string]bool{primaryProvider: true}
	for _, dm := range allDiscovered {
		if dm.Provider != "" && !seenProviders[dm.Provider] && isModelAllowed(providerAllows, dm.Provider, modelID) && !nonChat.matches(dm.Provider, modelID) {
			seenProviders[dm.Provider] = true
			// A provider-supplied pricing entry (rare today) wins over the
			// static baseline; static peak/cached still apply via the
			// AttachPricing overlay when a feed lacks them.
			variantPricing := dm.Pricing
			if variantPricing == nil {
				variantPricing = pricingEntryFromOverride(static.Pricing)
			}
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
				Pricing:  variantPricing,
			})
		}
	}
}

func pickMetadata(discovered DiscoveredModel, hasDiscovered bool, static config.ModelEntry, hasStatic bool) *ModelMetadata {
	var metadata *ModelMetadata
	if hasDiscovered && discovered.Metadata != nil {
		// AUD-67 (NENYA-308): the pointer belongs to the discovery catalog —
		// applyStaticEntryMetadata mutates in place, so handing it out
		// directly corrupted the SOURCE catalog (every later merge/reload
		// reads it). The merged entries own their clone.
		clone := *discovered.Metadata
		metadata = &clone
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

// pricingEntryFromOverride converts a static registry pricing override into
// the catalog's PricingEntry (USD), so billing and the cost guard see static
// pricing without an attached external feed. Returns nil for an IsZero
// override (the model has no static pricing).
func pricingEntryFromOverride(p config.PricingOverride) *PricingEntry {
	if p.IsZero() {
		return nil
	}
	return &PricingEntry{
		InputCostPer1M:       p.InputCostPer1M,
		OutputCostPer1M:      p.OutputCostPer1M,
		PeakInputCostPer1M:   p.PeakInputCostPer1M,
		PeakOutputCostPer1M:  p.PeakOutputCostPer1M,
		CachedInputCostPer1M: p.CachedInputCostPer1M,
		CacheWriteCostPer1M:  p.CacheWriteCostPer1M,
		Currency:             "USD",
	}
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
