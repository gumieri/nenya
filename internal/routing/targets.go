package routing

import (
	"log/slog"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"nenya/internal/config"
	"nenya/internal/discovery"
	"nenya/internal/infra"
	"nenya/internal/resilience"
)

// DefaultAgentCooldownSec is the default cooldown between requests to the
// same agent+provider+model combination when not explicitly configured.
const DefaultAgentCooldownSec = 60

// DefaultFailureThreshold is the default number of consecutive failures
// before a circuit breaker trips open.
const DefaultFailureThreshold = 5

// DefaultSuccessThreshold is the default number of consecutive successes
// required to close a half-open circuit breaker.
const DefaultSuccessThreshold = 1

// DefaultHalfOpenMaxRequests is the default maximum number of probe
// requests allowed while a circuit is in half-open state.
const DefaultHalfOpenMaxRequests = 3

const (
	latencyJitterPct = 0.10
)

// AgentState tracks per-model request counters, circuit breaker state,
// and cached selector resolution results.
type AgentState struct {
	Counters        map[string]uint64
	CB              *resilience.CircuitBreaker
	Mu              sync.Mutex
	selectorCache   map[string]selectorCacheEntry
	selectorCacheMu sync.RWMutex
}

type selectorCacheEntry struct {
	timestamp time.Time
	models    []config.AgentModel
	hash      string // hash of dynamic model entries for change detection
}

// NewAgentState creates an AgentState with a circuit breaker and
// optional state-change logging.
func NewAgentState(logger *slog.Logger) *AgentState {
	onChange := func(key string, from, to resilience.State) {
		switch to {
		case resilience.StateOpen:
			logger.Warn("[CIRCUIT BREAKER] "+key+" is DOWN/RATE-LIMITED. Tripping circuit. Skipping for 60s.",
				"from", from, "to", to)
		case resilience.StateHalfOpen:
			logger.Info("[CIRCUIT BREAKER] "+key+" probing upstream (HALF_OPEN).",
				"from", from, "to", to)
		case resilience.StateClosed:
			logger.Info("[CIRCUIT BREAKER] "+key+" recovered — circuit CLOSED.",
				"from", from, "to", to)
		}
	}

	return &AgentState{
		Counters:      make(map[string]uint64),
		selectorCache: make(map[string]selectorCacheEntry),
		CB: resilience.NewCircuitBreaker(
			DefaultFailureThreshold,
			DefaultSuccessThreshold,
			DefaultHalfOpenMaxRequests,
			time.Duration(DefaultAgentCooldownSec)*time.Second,
			onChange,
		),
	}
}

func (a *AgentState) BuildTargetList(logger *slog.Logger, agentName string, agent config.AgentConfig, tokenCount int, providers map[string]*config.Provider, catalog *discovery.ModelCatalog, autoContextSkip bool) []UpstreamTarget {
	models := a.expandModels(agentName, agent, catalog, providers, logger)
	if len(models) == 0 {
		return nil
	}

	n := len(models)
	start := a.selectStart(agent, agentName, n)

	active := make([]UpstreamTarget, 0, n)
	cooling := make([]UpstreamTarget, 0, n)

	for i := 0; i < n; i++ {
		t, ok := a.buildTarget(logger, agentName, models[(start+i)%n], tokenCount, providers, catalog, autoContextSkip)
		if !ok {
			continue
		}
		if a.CB.Peek(t.CoolKey) {
			active = append(active, *t)
		} else {
			cooling = append(cooling, *t)
		}
	}
	return append(active, cooling...)
}

func (a *AgentState) expandModels(agentName string, agent config.AgentConfig, catalog *discovery.ModelCatalog, providers map[string]*config.Provider, logger *slog.Logger) []config.AgentModel {
	hasDynamic := false
	hasDeferred := false
	hasProviderOnly := false
	for _, m := range agent.Models {
		if m.ProviderRgx != "" || m.ModelRgx != "" {
			hasDynamic = true
			break
		}
	}
	for _, m := range agent.Models {
		if m.Provider == "" && m.Model != "" {
			hasDeferred = true
			break
		}
	}
	for _, m := range agent.Models {
		if m.Provider != "" && m.Model == "" && m.ProviderRgx == "" && m.ModelRgx == "" {
			hasProviderOnly = true
			break
		}
	}
	if !hasDynamic && !hasDeferred && !hasProviderOnly {
		return agent.Models
	}

	models := agent.Models
	if hasProviderOnly {
		models = a.expandProviderOnly(models, catalog, providers, logger)
	}

	if hasDynamic {
		hash := hashDynamicModels(models)
		catTs := catalog.FetchedAt()

		a.selectorCacheMu.RLock()
		cached, hit := a.selectorCache[agentName]
		a.selectorCacheMu.RUnlock()
		if hit && cached.hash == hash && cached.timestamp.Equal(catTs) {
			return cached.models
		}

		models = expandDynamicModels(models, catalog, providers, logger)

		a.selectorCacheMu.Lock()
		a.selectorCache[agentName] = selectorCacheEntry{
			timestamp: catTs,
			models:    models,
			hash:      hash,
		}
		a.selectorCacheMu.Unlock()
	}

	if hasDeferred {
		models = a.expandDeferredProviders(models, catalog, providers, logger)
	}

	return models
}

func (a *AgentState) expandDeferredProviders(models []config.AgentModel, catalog *discovery.ModelCatalog, providers map[string]*config.Provider, logger *slog.Logger) []config.AgentModel {
	expanded := make([]config.AgentModel, 0, len(models)*2)
	for _, m := range models {
		if m.Provider != "" || m.Model == "" {
			expanded = append(expanded, m)
			continue
		}

		if catalog != nil {
			entries := catalog.LookupAll(m.Model)
			if len(entries) > 0 {
				for _, e := range entries {
					if _, ok := providers[e.Provider]; !ok {
						continue
					}
					am := config.AgentModel{
						Provider:   e.Provider,
						Model:      m.Model,
						MaxContext: m.MaxContext,
						MaxOutput:  m.MaxOutput,
					}
					if am.MaxContext <= 0 {
						am.MaxContext = e.MaxContext
					}
					if am.MaxOutput <= 0 {
						am.MaxOutput = e.MaxOutput
					}
					logger.Debug("expanded deferred provider",
						"model", m.Model, "provider", e.Provider,
						"max_context", am.MaxContext, "max_output", am.MaxOutput)
					expanded = append(expanded, am)
				}
				continue
			}
		}

		entry, ok := config.ModelRegistry[m.Model]
		if ok {
			if p, ok := providers[entry.Provider]; ok {
				am := config.AgentModel{
					Provider:   p.Name,
					Model:      m.Model,
					MaxContext: m.MaxContext,
					MaxOutput:  m.MaxOutput,
				}
				if am.MaxContext <= 0 {
					am.MaxContext = entry.MaxContext
				}
				if am.MaxOutput <= 0 {
					am.MaxOutput = entry.MaxOutput
				}
				logger.Debug("deferred provider fallback to ModelRegistry",
					"model", m.Model, "provider", p.Name)
				expanded = append(expanded, am)
				continue
			}
		}

		logger.Warn("deferred provider could not be resolved",
			"model", m.Model)
	}
	return expanded
}

func hashDynamicModels(models []config.AgentModel) string {
	var parts []string
	for _, m := range models {
		if m.ProviderRgx == "" && m.ModelRgx == "" {
			continue
		}
		parts = append(parts, m.ProviderRgx, m.ModelRgx)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "|")
}

func (a *AgentState) expandProviderOnly(models []config.AgentModel, catalog *discovery.ModelCatalog, providers map[string]*config.Provider, logger *slog.Logger) []config.AgentModel {
	expanded := make([]config.AgentModel, 0, len(models)*2)
	for _, m := range models {
		if !(m.Provider != "" && m.Model == "" && m.ProviderRgx == "" && m.ModelRgx == "") {
			expanded = append(expanded, m)
			continue
		}

		providerName := m.Provider
		if _, ok := providers[providerName]; !ok {
			logger.Warn("provider-only entry: provider not configured", "provider", providerName)
			continue
		}

		if catalog != nil {
			discModels := catalog.ModelsForProvider(providerName)
			if len(discModels) > 0 {
				for _, dm := range discModels {
					am := config.AgentModel{
						Provider:   dm.Provider,
						Model:      dm.ID,
						MaxContext: m.MaxContext,
						MaxOutput:  m.MaxOutput,
					}
					if am.MaxContext <= 0 {
						am.MaxContext = dm.MaxContext
					}
					if am.MaxOutput <= 0 {
						am.MaxOutput = dm.MaxOutput
					}
					logger.Debug("expanded provider-only entry from catalog",
						"provider", providerName, "model", dm.ID,
						"max_context", am.MaxContext, "max_output", am.MaxOutput)
					expanded = append(expanded, am)
				}
				continue
			}
		}

		registryModels := make([]config.AgentModel, 0)
		for modelID, entry := range config.ModelRegistry {
			if entry.Provider == providerName {
				am := config.AgentModel{
					Provider:   providerName,
					Model:      modelID,
					MaxContext: m.MaxContext,
					MaxOutput:  m.MaxOutput,
				}
				if am.MaxContext <= 0 {
					am.MaxContext = entry.MaxContext
				}
				if am.MaxOutput <= 0 {
					am.MaxOutput = entry.MaxOutput
				}
				registryModels = append(registryModels, am)
			}
		}
		if len(registryModels) > 0 {
			logger.Debug("expanded provider-only entry from ModelRegistry fallback",
				"provider", providerName, "count", len(registryModels))
			expanded = append(expanded, registryModels...)
			continue
		}

		logger.Warn("provider-only entry: no models found", "provider", providerName)
	}
	return expanded
}

func expandDynamicModels(models []config.AgentModel, catalog *discovery.ModelCatalog, providers map[string]*config.Provider, logger *slog.Logger) []config.AgentModel {
	expanded := make([]config.AgentModel, 0, len(models))
	for _, m := range models {
		if !m.IsDynamic() {
			expanded = append(expanded, m)
			continue
		}
		matchedAny := false
		for _, dm := range catalog.AllModels() {
			if !m.MatchesCatalog(dm.Provider, dm.ID) {
				continue
			}
			matchedAny = true
			provider, ok := providers[dm.Provider]
			if !ok {
				logger.Debug("provider from catalog not found", "provider", dm.Provider, "model", dm.ID)
				continue
			}
			am := config.AgentModel{
				Provider:   dm.Provider,
				Model:      dm.ID,
				URL:        provider.BaseURL,
				MaxContext: dm.MaxContext,
				MaxOutput:  dm.MaxOutput,
			}
			expanded = append(expanded, am)
		}
		if !matchedAny {
			logger.Warn("dynamic model entry matched no catalog entries",
				"provider_rgx", m.ProviderRgx, "model_rgx", m.ModelRgx)
		}
	}
	return expanded
}

func (a *AgentState) selectStart(agent config.AgentConfig, agentName string, n int) int {
	a.Mu.Lock()
	defer a.Mu.Unlock()

	if agent.Strategy == "fallback" {
		return 0
	}

	start := int(a.Counters[agentName]) % n
	a.Counters[agentName]++
	return start
}

func (a *AgentState) buildTarget(logger *slog.Logger, agentName string, m config.AgentModel, tokenCount int, providers map[string]*config.Provider, catalog *discovery.ModelCatalog, autoContextSkip bool) (*UpstreamTarget, bool) {
	maxCtx := resolveMaxContext(m, catalog)
	if maxCtx > 0 && tokenCount > maxCtx {
		logger.Info("skipping model: exceeds max_context",
			"model", m.Model, "max_context", maxCtx, "tokens", tokenCount)
		return nil, false
	}

	if autoContextSkip && maxCtx > 0 && tokenCount > 0 && maxCtx < tokenCount*2 {
		logger.Info("skipping model: context headroom too small",
			"model", m.Model, "max_context", maxCtx, "tokens", tokenCount)
		return nil, false
	}

	if !checkCapabilities(m, catalog) {
		return nil, false
	}

	p := ProviderURL(m.Provider, m.URL, providers)
	if p == "" {
		logger.Warn("unknown provider, skipping model", "provider", m.Provider, "model", m.Model)
		return nil, false
	}

	maxOut := resolveMaxOutput(m, catalog)

	return &UpstreamTarget{
		URL:        p,
		Model:      m.Model,
		CoolKey:    agentName + ":" + m.Provider + ":" + m.Model,
		Provider:   m.Provider,
		MaxOutput:  maxOut,
		MaxContext: maxCtx,
	}, true
}

func (a *AgentState) ActivateCooldown(target UpstreamTarget, cooldownDuration time.Duration) {
	a.CB.ForceOpen(target.CoolKey, cooldownDuration)
}

func (a *AgentState) RecordFailure(target UpstreamTarget, cooldownDuration time.Duration) {
	if target.CoolKey == "" {
		return
	}
	a.CB.RecordFailure(target.CoolKey, cooldownDuration)
}

func (a *AgentState) RecordSuccess(key string) {
	a.CB.RecordSuccess(key)
}

func (a *AgentState) ActiveCooldowns() int {
	return a.CB.ActiveCount()
}

func (a *AgentState) CBSnapshot() map[string]string {
	return a.CB.Snapshot()
}

func resolveMaxContext(m config.AgentModel, catalog *discovery.ModelCatalog) int {
	if m.MaxContext > 0 {
		return m.MaxContext
	}

	if catalog != nil {
		if disc, ok := catalog.Lookup(m.Model); ok && disc.MaxContext > 0 {
			return disc.MaxContext
		}
	}

	if entry, ok := config.ModelRegistry[m.Model]; ok && entry.MaxContext > 0 {
		return entry.MaxContext
	}

	return 0
}

func resolveMaxOutput(m config.AgentModel, catalog *discovery.ModelCatalog) int {
	if m.MaxOutput > 0 {
		return m.MaxOutput
	}

	if catalog != nil {
		if disc, ok := catalog.Lookup(m.Model); ok && disc.MaxOutput > 0 {
			return disc.MaxOutput
		}
	}

	if entry, ok := config.ModelRegistry[m.Model]; ok && entry.MaxOutput > 0 {
		return entry.MaxOutput
	}

	return 0
}

func checkCapabilities(m config.AgentModel, catalog *discovery.ModelCatalog) bool {
	if len(m.RequiredCapabilities) == 0 {
		return true
	}

	if catalog == nil {
		return true
	}

	dm, hasMeta := catalog.Lookup(m.Model)
	if !hasMeta || dm.Metadata == nil {
		return false
	}

	return modelHasCapabilities(dm.Metadata, m.RequiredCapabilities)
}

func ResolveWindowMaxContext(modelName string, agents map[string]config.AgentConfig, catalog *discovery.ModelCatalog) int {
	agent, ok := agents[modelName]
	if !ok {
		return 0
	}

	maxCtx := 0
	for _, m := range agent.Models {
		mc := resolveMaxContext(m, catalog)
		if mc > maxCtx {
			maxCtx = mc
		}
	}
	return maxCtx
}

func SortTargetsByLatency(targets []UpstreamTarget, latencyTracker *infra.LatencyTracker, jitterFn func() float64) []UpstreamTarget {
	if latencyTracker == nil || len(targets) <= 1 {
		return targets
	}

	if jitterFn == nil {
		jitterFn = rand.Float64
	}

	sorted := make([]UpstreamTarget, len(targets))
	copy(sorted, targets)

	sort.SliceStable(sorted, func(i, j int) bool {
		latencyI, okI := latencyTracker.Get(sorted[i].Model, sorted[i].Provider)
		latencyJ, okJ := latencyTracker.Get(sorted[j].Model, sorted[j].Provider)

		if !okI && !okJ {
			return false
		}
		if !okI {
			return false
		}
		if !okJ {
			return true
		}

		jitterI := latencyI.MedianMs * (1.0 + (jitterFn()*latencyJitterPct - latencyJitterPct/2))
		jitterJ := latencyJ.MedianMs * (1.0 + (jitterFn()*latencyJitterPct - latencyJitterPct/2))

		return jitterI < jitterJ
	})

	return sorted
}

func modelHasCapabilities(meta *discovery.ModelMetadata, required []string) bool {
	for _, cap := range required {
		switch cap {
		case "vision":
			if !meta.SupportsVision {
				return false
			}
		case "tool_calls":
			if !meta.SupportsToolCalls {
				return false
			}
		case "reasoning":
			if !meta.SupportsReasoning {
				return false
			}
		case "content_arrays":
			if !meta.SupportsContentArrays {
				return false
			}
		case "stream_options":
			if !meta.SupportsStreamOptions {
				return false
			}
		case "auto_tool_choice":
			if !meta.SupportsAutoToolChoice {
				return false
			}
		}
	}
	return true
}
