package discovery

import (
	"log/slog"
	"sync"
	"time"

	"nenya/internal/config"
)

const (
	HealthStatusOK          = "ok"
	HealthStatusUnreachable = "unreachable"
	HealthStatusEmpty       = "empty"
	HealthStatusInvalid     = "invalid"
	HealthStatusDegraded    = "degraded"
)

type ProviderHealth struct {
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	ModelsFound int       `json:"models_found"`
	LastFetched time.Time `json:"last_fetched"`
	Error       string    `json:"error,omitempty"`
}

type HealthRegistry struct {
	mu     sync.RWMutex
	health map[string]ProviderHealth
}

func NewHealthRegistry() *HealthRegistry {
	return &HealthRegistry{
		health: make(map[string]ProviderHealth),
	}
}

func (h *HealthRegistry) Update(provider string, status ProviderHealth) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.health[provider] = status
}

func (h *HealthRegistry) Get(provider string) (ProviderHealth, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	health, ok := h.health[provider]
	return health, ok
}

func (h *HealthRegistry) Snapshot() map[string]ProviderHealth {
	h.mu.RLock()
	defer h.mu.RUnlock()
	snapshot := make(map[string]ProviderHealth, len(h.health))
	for k, v := range h.health {
		snapshot[k] = v
	}
	return snapshot
}

func ValidateProviderHealth(providerName string, provider *config.Provider, catalog *ModelCatalog, logger *slog.Logger) ProviderHealth {
	health := ProviderHealth{
		Name:        providerName,
		LastFetched: time.Now(),
	}

	if provider.APIKey == "" && provider.AuthStyle != "none" {
		health.Status = HealthStatusUnreachable
		health.Error = "no API key configured"
		return health
	}

	models := catalog.ModelsForProvider(providerName)
	health.ModelsFound = len(models)

	if len(models) == 0 {
		health.Status = HealthStatusEmpty
		health.Error = "no models discovered"
		return health
	}

	health.Status = HealthStatusOK
	return health
}

func ValidateAllProviders(providers map[string]*config.Provider, catalog *ModelCatalog, logger *slog.Logger) *HealthRegistry {
	registry := NewHealthRegistry()

	for name, p := range providers {
		health := ValidateProviderHealth(name, p, catalog, logger)
		registry.Update(name, health)

		switch health.Status {
		case HealthStatusOK:
			logger.Info("provider health check passed", "provider", name, "models", health.ModelsFound)
		case HealthStatusUnreachable:
			logger.Warn("provider health check failed", "provider", name, "status", health.Status, "error", health.Error)
		case HealthStatusEmpty:
			logger.Warn("provider returned no models", "provider", name)
		}
	}

	return registry
}
