package discovery

import (
	"sync"
	"time"
)

type DiscoveredModel struct {
	ID         string
	Provider   string
	MaxContext int
	MaxOutput  int
	OwnedBy    string
}

type ModelCatalog struct {
	mu        sync.RWMutex
	models    map[string]DiscoveredModel
	providers map[string][]string
	fetchedAt time.Time
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
	c.models[model.ID] = model
	c.providers[model.Provider] = append(c.providers[model.Provider], model.ID)
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
