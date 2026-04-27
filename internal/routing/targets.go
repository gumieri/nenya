package routing

import (
	"fmt"
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
	Counters           map[string]uint64
	CB                 *resilience.CircuitBreaker
	Mu                 sync.Mutex
	selectorCache      map[string]selectorCacheEntry
	selectorCacheMu    sync.RWMutex
}

type selectorCacheEntry struct {
	timestamp  time.Time
	models     []config.AgentModel
	agentHash  string // hash of agent.ModelSelectors for change detection
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
	var models []config.AgentModel
	n := len(agent.Models)
	hasSelectors := len(agent.ModelSelectors) > 0 && catalog != nil

	if hasSelectors {
		selectorModels, err := a.getCachedSelectorModels(agentName, agent, catalog, providers, logger)
		if err != nil {
			logger.Warn("failed to resolve model selectors, falling back to static models",
				"agent", agentName, "error", err)
		} else if len(selectorModels) > 0 {
			models = selectorModels
			n = len(models)
		}
	}

	if models == nil {
		n = len(agent.Models)
		models = agent.Models
		if n == 0 {
			return nil
		}
	}

	a.Mu.Lock()
	var start int
	if agent.Strategy != "" && agent.Strategy == "fallback" {
		start = 0
	} else {
		start = int(a.Counters[agentName]) % n
		a.Counters[agentName]++
	}
	a.Mu.Unlock()

	active := make([]UpstreamTarget, 0, n)
	cooling := make([]UpstreamTarget, 0, n)

	for i := 0; i < n; i++ {
		m := models[(start+i)%n]

		maxCtx := m.MaxContext
		if maxCtx == 0 {
			if catalog != nil {
				if disc, ok := catalog.Lookup(m.Model); ok && disc.MaxContext > 0 {
					maxCtx = disc.MaxContext
				}
			}
			if maxCtx == 0 {
				if entry, ok := config.ModelRegistry[m.Model]; ok && entry.MaxContext > 0 {
					maxCtx = entry.MaxContext
				}
			}
		}

		if maxCtx > 0 && tokenCount > maxCtx {
			logger.Info("skipping model: exceeds max_context",
				"model", m.Model, "max_context", maxCtx, "tokens", tokenCount)
			continue
		}

		if autoContextSkip && maxCtx > 0 && tokenCount > 0 && maxCtx < tokenCount*2 {
			logger.Info("skipping model: context headroom too small",
				"model", m.Model, "max_context", maxCtx, "tokens", tokenCount)
			continue
		}

		if len(m.RequiredCapabilities) > 0 && catalog != nil {
			dm, hasMeta := catalog.Lookup(m.Model)
			if !hasMeta || dm.Metadata == nil {
				logger.Debug("skipping model: no metadata for capability check",
					"model", m.Model, "required", m.RequiredCapabilities)
				continue
			}
			if !modelHasCapabilities(dm.Metadata, m.RequiredCapabilities) {
				logger.Debug("skipping model: missing required capabilities",
					"model", m.Model, "required", m.RequiredCapabilities)
				continue
			}
		}

		p := ProviderURL(m.Provider, m.URL, providers)
		if p == "" {
			logger.Warn("unknown provider, skipping model", "provider", m.Provider, "model", m.Model)
			continue
		}

		maxOut := m.MaxOutput
		if maxOut == 0 {
			if catalog != nil {
				if disc, ok := catalog.Lookup(m.Model); ok && disc.MaxOutput > 0 {
					maxOut = disc.MaxOutput
				}
			}
			if maxOut == 0 {
				if entry, ok := config.ModelRegistry[m.Model]; ok && entry.MaxOutput > 0 {
					maxOut = entry.MaxOutput
				}
			}
		}

		t := UpstreamTarget{
			URL:        p,
			Model:      m.Model,
			CoolKey:    agentName + ":" + m.Provider + ":" + m.Model,
			Provider:   m.Provider,
			MaxOutput:  maxOut,
			MaxContext: maxCtx,
		}
		// Use Peek (no side effects) here; Allow is called later in
		// prepareAndSend when the request is actually dispatched.
		if a.CB.Peek(t.CoolKey) {
			active = append(active, t)
		} else {
			cooling = append(cooling, t)
		}
	}
	return append(active, cooling...)
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

func (a *AgentState) getCachedSelectorModels(agentName string, agent config.AgentConfig, catalog *discovery.ModelCatalog, providers map[string]*config.Provider, logger *slog.Logger) ([]config.AgentModel, error) {
	selectorHash := hashSelectors(agent.ModelSelectors)
	catalogTimestamp := catalog.FetchedAt()

	a.selectorCacheMu.RLock()
	cached, hit := a.selectorCache[agentName]
	a.selectorCacheMu.RUnlock()

	if hit && cached.agentHash == selectorHash && cached.timestamp.Equal(catalogTimestamp) {
		return cached.models, nil
	}

	models, err := resolveSelectorModels(agentName, agent, catalog, providers, logger)
	if err != nil {
		return nil, err
	}

	if len(models) > 0 {
		a.selectorCacheMu.Lock()
		a.selectorCache[agentName] = selectorCacheEntry{
			timestamp: catalogTimestamp,
			models:    models,
			agentHash: selectorHash,
		}
		a.selectorCacheMu.Unlock()
	}

	return models, nil
}

func hashSelectors(selectors []config.AgentModelSelector) string {
	if len(selectors) == 0 {
		return ""
	}

	var parts []string
	for _, sel := range selectors {
		parts = append(parts, sel.ProviderRgx, sel.ModelRgx, strings.Join(sel.Models, ","))
	}
	return strings.Join(parts, "|")
}

func ResolveWindowMaxContext(modelName string, agents map[string]config.AgentConfig, catalog *discovery.ModelCatalog) int {
	if agent, ok := agents[modelName]; ok {
		maxCtx := 0
		for _, m := range agent.Models {
			mc := m.MaxContext
			if mc == 0 {
				if catalog != nil {
					if disc, ok := catalog.Lookup(m.Model); ok && disc.MaxContext > 0 {
						mc = disc.MaxContext
					}
				}
				if mc == 0 {
					if entry, ok := config.ModelRegistry[m.Model]; ok && entry.MaxContext > 0 {
						mc = entry.MaxContext
					}
				}
			}
			if mc > maxCtx {
				maxCtx = mc
			}
		}
		return maxCtx
	}
	return 0
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

func resolveSelectorModels(agentName string, agent config.AgentConfig, catalog *discovery.ModelCatalog, providers map[string]*config.Provider, logger *slog.Logger) ([]config.AgentModel, error) {
	modelIDs, err := resolveAgentSelectors(agent, catalog)
	if err != nil {
		return nil, err
	}

	if len(modelIDs) == 0 {
		return nil, nil
	}

	models := make([]config.AgentModel, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		dm, ok := catalog.Lookup(modelID)
		if !ok {
			logger.Debug("model from selector not found in catalog", "model", modelID)
			continue
		}

		provider, ok := providers[dm.Provider]
		if !ok {
			logger.Debug("provider from selector not found", "provider", dm.Provider, "model", modelID)
			continue
		}

		am := config.AgentModel{
			Provider:   dm.Provider,
			Model:      dm.ID,
			URL:        provider.BaseURL,
			MaxContext: dm.MaxContext,
			MaxOutput:  dm.MaxOutput,
		}
		models = append(models, am)
	}

	if len(models) == 0 {
		return nil, nil
	}

	return models, nil
}

func resolveAgentSelectors(agent config.AgentConfig, catalog *discovery.ModelCatalog) ([]string, error) {
	if len(agent.ModelSelectors) == 0 {
		return nil, nil
	}

	for i := range agent.ModelSelectors {
		sel := &agent.ModelSelectors[i]
		if err := sel.Compile(); err != nil {
			return nil, fmt.Errorf("selector %d: %w", i, err)
		}
	}

	matches := []string{}
	for _, sel := range agent.ModelSelectors {
		for _, dm := range catalog.AllModels() {
			if !sel.Matches(dm.Provider, dm.ID) {
				continue
			}
			matches = append(matches, dm.ID)
		}

		if len(matches) == 0 {
			continue
		}

		if len(sel.Models) > 0 {
			whitelist := make(map[string]struct{}, len(sel.Models))
			for _, m := range sel.Models {
				whitelist[m] = struct{}{}
			}
			filtered := make([]string, 0, len(matches))
			for _, m := range matches {
				if _, ok := whitelist[m]; ok {
					filtered = append(filtered, m)
				}
			}
			matches = filtered
		}

		if len(matches) > 0 {
			return matches, nil
		}

		matches = nil
	}

	return nil, nil
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
