package discovery

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"nenya/internal/config"
)

const (
	defaultMaxConcurrentChecks = 5
)

type HealthCheckResult struct {
	Provider         string
	Status           string
	ModelsFound      int
	ExpectedModels   []string
	MissingModels    []string
	NewModels        []string
	DeprecatedModels []string
	LastFetched      time.Time
	Error            string
	ResponseTime     time.Duration
}

type HealthCheckConfig struct {
	Timeout           time.Duration
	MaxRetries        int
	RetryDelay        time.Duration
	EnableDriftCheck  bool
	DriftWarningLevel int
	MaxConcurrent     int
}

type AdvancedHealthChecker struct {
	config   HealthCheckConfig
	catalog  *ModelCatalog
	registry map[string]config.ModelEntry
	logger   *slog.Logger
	results  map[string]HealthCheckResult
	mu       sync.RWMutex
}

func NewAdvancedHealthChecker(config HealthCheckConfig, catalog *ModelCatalog, registry map[string]config.ModelEntry, logger *slog.Logger) *AdvancedHealthChecker {
	if config.Timeout == 0 {
		config.Timeout = 10 * time.Second
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 3
	}
	if config.RetryDelay == 0 {
		config.RetryDelay = 1 * time.Second
	}
	if config.DriftWarningLevel == 0 {
		config.DriftWarningLevel = 5
	}
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = defaultMaxConcurrentChecks
	}

	return &AdvancedHealthChecker{
		config:   config,
		catalog:  catalog,
		registry: registry,
		logger:   logger,
		results:  make(map[string]HealthCheckResult),
	}
}

func (h *AdvancedHealthChecker) CheckProvider(ctx context.Context, provider string, fetchFunc func(context.Context) ([]DiscoveredModel, error)) HealthCheckResult {
	start := time.Now()
	result := HealthCheckResult{
		Provider:     provider,
		LastFetched:  time.Now(),
		ResponseTime: 0,
	}

	var discoveredModels []DiscoveredModel
	var fetchErr error

	for attempt := 0; attempt <= h.config.MaxRetries; attempt++ {
		if attempt > 0 {
			h.logger.Debug("retrying provider health check",
				"provider", provider,
				"attempt", attempt,
				"max_retries", h.config.MaxRetries)

			select {
			case <-time.After(h.config.RetryDelay):
			case <-ctx.Done():
				result.Status = HealthStatusUnreachable
				result.Error = ctx.Err().Error()
				return result
			}
		}

		fetchCtx, cancel := context.WithTimeout(ctx, h.config.Timeout)
		discoveredModels, fetchErr = fetchFunc(fetchCtx)
		cancel()

		if fetchErr == nil {
			break
		}

		h.logger.Warn("provider health check failed",
			"provider", provider,
			"attempt", attempt,
			"error", fetchErr)
	}

	result.ResponseTime = time.Since(start)

	if fetchErr != nil {
		result.Status = HealthStatusUnreachable
		result.Error = fetchErr.Error()
		return result
	}

	if len(discoveredModels) == 0 {
		result.Status = HealthStatusEmpty
		result.Error = "no models returned"
		return result
	}

	result.ModelsFound = len(discoveredModels)
	result.Status = HealthStatusOK

	if h.config.EnableDriftCheck {
		h.analyzeDrift(provider, discoveredModels, &result)
	}

	h.mu.Lock()
	h.results[provider] = result
	h.mu.Unlock()

	return result
}

func (h *AdvancedHealthChecker) analyzeDrift(provider string, discoveredModels []DiscoveredModel, result *HealthCheckResult) {
	if h.registry == nil {
		return
	}

	expectedModels := make([]string, 0)
	for modelID, entry := range h.registry {
		if entry.Provider == provider {
			expectedModels = append(expectedModels, modelID)
		}
	}
	result.ExpectedModels = expectedModels

	discoveredSet := make(map[string]bool)
	for _, model := range discoveredModels {
		discoveredSet[model.ID] = true
	}

	expectedSet := make(map[string]bool)
	for _, model := range expectedModels {
		expectedSet[model] = true
	}

	for _, model := range expectedModels {
		if !discoveredSet[model] {
			result.MissingModels = append(result.MissingModels, model)
		}
	}

	for _, model := range discoveredModels {
		if !expectedSet[model.ID] {
			result.NewModels = append(result.NewModels, model.ID)
		}
	}

	if len(result.MissingModels) > 0 {
		result.Status = HealthStatusDegraded

		if len(result.MissingModels) >= h.config.DriftWarningLevel {
			h.logger.Warn("provider has significant model drift",
				"provider", provider,
				"missing_count", len(result.MissingModels),
				"missing_models", result.MissingModels)
		}
	}

	if len(result.NewModels) > 0 {
		h.logger.Info("provider has new models",
			"provider", provider,
			"new_count", len(result.NewModels),
			"new_models", result.NewModels)
	}
}

func (h *AdvancedHealthChecker) CheckAllProviders(ctx context.Context, providers map[string]func(context.Context) ([]DiscoveredModel, error)) map[string]HealthCheckResult {
	results := make(map[string]HealthCheckResult)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, h.config.MaxConcurrent)

	for provider, fetchFunc := range providers {
		wg.Add(1)
		go func(p string, f func(context.Context) ([]DiscoveredModel, error)) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			result := h.CheckProvider(ctx, p, f)

			mu.Lock()
			results[p] = result
			mu.Unlock()
		}(provider, fetchFunc)
	}

	wg.Wait()

	h.mu.Lock()
	for provider, result := range results {
		h.results[provider] = result
	}
	h.mu.Unlock()

	return results
}

func (h *AdvancedHealthChecker) GetResult(provider string) (HealthCheckResult, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	result, exists := h.results[provider]
	return result, exists
}

func (h *AdvancedHealthChecker) GetAllResults() map[string]HealthCheckResult {
	h.mu.RLock()
	defer h.mu.RUnlock()

	results := make(map[string]HealthCheckResult, len(h.results))
	for k, v := range h.results {
		results[k] = v
	}

	return results
}

func (h *AdvancedHealthChecker) GetSummary() map[string]interface{} {
	h.mu.RLock()
	defer h.mu.RUnlock()

	summary := map[string]interface{}{
		"total_providers": len(h.results),
		"ok":              0,
		"degraded":        0,
		"unreachable":     0,
		"empty":           0,
		"invalid":         0,
		"total_models":    0,
		"missing_models":  0,
		"new_models":      0,
	}

	for _, result := range h.results {
		switch result.Status {
		case HealthStatusOK:
			summary["ok"] = summary["ok"].(int) + 1
		case HealthStatusDegraded:
			summary["degraded"] = summary["degraded"].(int) + 1
		case HealthStatusUnreachable:
			summary["unreachable"] = summary["unreachable"].(int) + 1
		case HealthStatusEmpty:
			summary["empty"] = summary["empty"].(int) + 1
		case HealthStatusInvalid:
			summary["invalid"] = summary["invalid"].(int) + 1
		}

		summary["total_models"] = summary["total_models"].(int) + result.ModelsFound
		summary["missing_models"] = summary["missing_models"].(int) + len(result.MissingModels)
		summary["new_models"] = summary["new_models"].(int) + len(result.NewModels)
	}

	return summary
}

func (h *AdvancedHealthChecker) GetDriftReport() map[string]interface{} {
	h.mu.RLock()
	defer h.mu.RUnlock()

	report := make(map[string]interface{})
	driftedProviders := make([]string, 0)

	for provider, result := range h.results {
		if len(result.MissingModels) > 0 || len(result.NewModels) > 0 {
			driftedProviders = append(driftedProviders, provider)

			providerReport := map[string]interface{}{
				"status":           result.Status,
				"missing_count":    len(result.MissingModels),
				"missing_models":   result.MissingModels,
				"new_count":        len(result.NewModels),
				"new_models":       result.NewModels,
				"last_checked":     result.LastFetched,
				"response_time_ms": result.ResponseTime.Milliseconds(),
			}

			report[provider] = providerReport
		}
	}

	report["drifted_providers"] = driftedProviders
	report["drifted_count"] = len(driftedProviders)

	return report
}

func (h *AdvancedHealthChecker) ClearResults() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.results = make(map[string]HealthCheckResult)
}

func (h *AdvancedHealthChecker) IsProviderHealthy(provider string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	result, exists := h.results[provider]
	if !exists {
		return false
	}

	return result.Status == HealthStatusOK || result.Status == HealthStatusDegraded
}

func (h *AdvancedHealthChecker) GetUnhealthyProviders() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	unhealthy := make([]string, 0)
	for provider, result := range h.results {
		if result.Status != HealthStatusOK && result.Status != HealthStatusDegraded {
			unhealthy = append(unhealthy, provider)
		}
	}

	return unhealthy
}

func (h *AdvancedHealthChecker) LogSummary() {
	summary := h.GetSummary()

	h.logger.Info("health check summary",
		"total_providers", summary["total_providers"],
		"ok", summary["ok"],
		"degraded", summary["degraded"],
		"unreachable", summary["unreachable"],
		"empty", summary["empty"],
		"invalid", summary["invalid"],
		"total_models", summary["total_models"],
		"missing_models", summary["missing_models"],
		"new_models", summary["new_models"])

	if summary["degraded"].(int) > 0 {
		driftReport := h.GetDriftReport()
		h.logger.Warn("provider drift detected",
			"drifted_providers", driftReport["drifted_providers"],
			"drifted_count", driftReport["drifted_count"])
	}
}
