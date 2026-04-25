package discovery

import (
	"sync"
	"time"
)

type DiscoveredModel struct {
	ID         string `json:"id"`
	Provider   string `json:"provider"`
	MaxContext int    `json:"max_context"`
	MaxOutput  int    `json:"max_output"`
	OwnedBy    string `json:"owned_by"`
	Metadata   *ModelMetadata `json:"metadata,omitempty"` // Added metadata field
}

type ModelCatalog struct {
	mu           sync.RWMutex
	models       map[string]DiscoveredModel
	providers    map[string][]string
	fetchedAt    time.Time
	hasMetadata  bool
}

func NewModelCatalog() *ModelCatalog {
	return &ModelCatalog{
		models:    make(map[string]DiscoveredModel),
		providers: make(map[string][]string),
		fetchedAt: time.Now(),
	}
}

func (c *ModelCatalog) Lookup(modelID string) (DiscoveredModel, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.models[modelID]
	return m, ok
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
		if m, ok := c.models[id]; ok {
			models = append(models, m)
		}
	}
	return models
}

func (c *ModelCatalog) AllModels() []DiscoveredModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	models := make([]DiscoveredModel, 0, len(c.models))
	for _, m := range c.models {
		models = append(models, m)
	}
	return models
}

func (c *ModelCatalog) Add(model DiscoveredModel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if model.Metadata != nil && (model.Metadata.ScoreBonus != 0 ||
		model.Metadata.SupportsToolCalls || model.Metadata.SupportsReasoning ||
		model.Metadata.SupportsVision || model.Metadata.SupportsContentArrays) {
		c.hasMetadata = true
	}
	c.models[model.ID] = model
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
