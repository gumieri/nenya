package discovery

import (
	"sync"
	"time"
)

type DiscoveredModel struct {
	ID           string         `json:"id"`
	Provider     string         `json:"provider"`
	Format       string         `json:"format,omitempty"`
	MaxContext   int            `json:"max_context"`
	MaxOutput    int            `json:"max_output"`
	OwnedBy      string         `json:"owned_by"`
	Metadata     *ModelMetadata `json:"metadata,omitempty"`
	Pricing      *PricingEntry  `json:"pricing,omitempty"`
	ServiceKinds []string       `json:"service_kinds,omitempty"` // Future: use for intelligent model routing to embedding/TTS endpoints
}

func (m DiscoveredModel) HasCapability(c Capability) bool {
	return m.Metadata.HasCapability(c)
}

// MaxContext is the model's maximum context window in tokens.
// A value > 0 indicates a known limit. Values <= 0 are treated as unknown,
// disabling proactive truncation. Upstream providers may still reject
// payloads with context_length_exceeded errors (triggers retry with summarization).

type ModelCatalog struct {
	mu          sync.RWMutex
	models      map[string][]DiscoveredModel
	providers   map[string][]string
	fetchedAt   time.Time
	hasMetadata bool
}

func NewModelCatalog() *ModelCatalog {
	return &ModelCatalog{
		models:    make(map[string][]DiscoveredModel),
		providers: make(map[string][]string),
		fetchedAt: time.Now(),
	}
}

func (c *ModelCatalog) Lookup(modelID string) (DiscoveredModel, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entries, ok := c.models[modelID]
	if !ok || len(entries) == 0 {
		return DiscoveredModel{}, false
	}
	return entries[0], true
}

func (c *ModelCatalog) LookupAll(modelID string) []DiscoveredModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entries, ok := c.models[modelID]
	if !ok {
		return nil
	}
	result := make([]DiscoveredModel, len(entries))
	copy(result, entries)
	return result
}

func (c *ModelCatalog) ModelsForProvider(provider string) []DiscoveredModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ids, ok := c.providers[provider]
	if !ok {
		return nil
	}
	models := make([]DiscoveredModel, 0, len(ids))
	for _, id := range ids {
		if entries, ok := c.models[id]; ok {
			for _, m := range entries {
				if m.Provider == provider {
					models = append(models, m)
				}
			}
		}
	}
	return models
}

func (c *ModelCatalog) AllModels() []DiscoveredModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var total int
	for _, entries := range c.models {
		total += len(entries)
	}
	models := make([]DiscoveredModel, 0, total)
	for _, entries := range c.models {
		models = append(models, entries...)
	}
	return models
}

func (c *ModelCatalog) Add(model DiscoveredModel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if model.Metadata != nil && (model.Metadata.ScoreBonus != 0 ||
		model.Metadata.SupportsToolCalls || model.Metadata.SupportsReasoning ||
		model.Metadata.SupportsVision || model.Metadata.SupportsContentArrays ||
		model.Metadata.Pricing != nil) {
		c.hasMetadata = true
	}
	c.models[model.ID] = append(c.models[model.ID], model)
	c.providers[model.Provider] = append(c.providers[model.Provider], model.ID)
}

func (c *ModelCatalog) HasMetadata() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hasMetadata
}

func (c *ModelCatalog) FetchedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fetchedAt
}

func (c *ModelCatalog) UpdateFetchedAt(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetchedAt = t
}

// Remove deletes every model entry for which match returns true, keeping the
// models and providers indexes consistent. Used to drop non-chat models from
// an externally-built catalog (see ApplyProviderNonChatToCatalog).
func (c *ModelCatalog) Remove(match func(DiscoveredModel) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, entries := range c.models {
		kept := entries[:0]
		for _, m := range entries {
			if !match(m) {
				kept = append(kept, m)
			}
		}
		if len(kept) == 0 {
			delete(c.models, id)
		} else {
			c.models[id] = kept
		}
	}
	for provider, ids := range c.providers {
		kept := ids[:0]
		for _, id := range ids {
			if _, ok := c.models[id]; ok {
				kept = append(kept, id)
			}
		}
		if len(kept) == 0 {
			delete(c.providers, provider)
		} else {
			c.providers[provider] = kept
		}
	}
}

func (c *ModelCatalog) AttachPricing(pricing map[string]PricingEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, p := range pricing {
		if entries, ok := c.models[id]; ok {
			for i := range entries {
				entries[i].Pricing = &p
			}
			c.models[id] = entries
		}
	}
}
