package routing

import (
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"nenya/config"
	"nenya/internal/discovery"
	"nenya/internal/infra"
	"nenya/internal/resilience"
	"nenya/internal/util"
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

// AgentState tracks per-model request counters, circuit breaker state,
// and cached selector resolution results.
type AgentState struct {
	Counters        map[string]uint64
	CB              *resilience.CircuitBreaker
	Metrics         *infra.Metrics
	mu              sync.Mutex
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
func NewAgentState(logger *slog.Logger, metrics *infra.Metrics) *AgentState {
	return NewAgentStateWithConfig(logger, metrics, nil)
}

// NewAgentStateWithConfig creates an AgentState with a circuit breaker using
// governance configuration for halfOpenMaxRequests.
func NewAgentStateWithConfig(logger *slog.Logger, metrics *infra.Metrics, govConfig *config.GovernanceConfig) *AgentState {
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

	// Default halfOpenMaxRequests is 3, but can be overridden by governance config
	halfOpenMaxRequests := uint32(3)
	if govConfig != nil && govConfig.HalfOpenMaxRequests > 0 {
		halfOpenMaxRequests = uint32(govConfig.HalfOpenMaxRequests)
	}

	as := &AgentState{
		Counters:      make(map[string]uint64),
		Metrics:       metrics,
		selectorCache: make(map[string]selectorCacheEntry),
		CB: resilience.NewCircuitBreaker(
			DefaultFailureThreshold,
			DefaultSuccessThreshold,
			halfOpenMaxRequests,
			time.Duration(DefaultAgentCooldownSec)*time.Second,
			onChange,
		),
	}

	if metrics != nil {
		as.CB.SetStateChangeMetricCallback(func(key, from, to string) {
			metrics.RecordCBStateTransition(key, from, to)
		})
	}

	return as
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

type modelFlags struct {
	hasDynamic      bool
	hasDeferred     bool
	hasProviderOnly bool
}

func detectModelFlags(models []config.AgentModel) modelFlags {
	var flags modelFlags
	for _, m := range models {
		if m.ProviderRgx != "" || m.ModelRgx != "" {
			flags.hasDynamic = true
		}
		if m.Provider == "" && m.Model != "" {
			flags.hasDeferred = true
		}
		if m.Provider != "" && m.Model == "" && m.ProviderRgx == "" && m.ModelRgx == "" {
			flags.hasProviderOnly = true
		}
	}
	return flags
}

func (a *AgentState) expandModels(agentName string, agent config.AgentConfig, catalog *discovery.ModelCatalog, providers map[string]*config.Provider, logger *slog.Logger) []config.AgentModel {
	flags := detectModelFlags(agent.Models)
	if !flags.hasDynamic && !flags.hasDeferred && !flags.hasProviderOnly {
		return agent.Models
	}

	models := agent.Models
	if flags.hasProviderOnly {
		models = a.expandProviderOnly(models, catalog, providers, logger)
	}

	if flags.hasDynamic {
		models = a.expandDynamicWithCache(agentName, models, catalog, providers, logger)
	}

	if flags.hasDeferred {
		models = a.expandDeferredProviders(models, catalog, providers, logger)
	}

	return models
}

func (a *AgentState) expandDynamicWithCache(agentName string, models []config.AgentModel, catalog *discovery.ModelCatalog, providers map[string]*config.Provider, logger *slog.Logger) []config.AgentModel {
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
	return models
}

// buildAgentModelWithFallback constructs an AgentModel for a discovered model,
// using fallback values from the discovery entry when agent model fields are zero.
func buildAgentModelWithFallback(modelName string, base config.AgentModel, disc discovery.DiscoveredModel) config.AgentModel {
	am := config.AgentModel{
		Provider:   disc.Provider,
		Model:      modelName,
		MaxContext: base.MaxContext,
		MaxOutput:  base.MaxOutput,
	}
	if am.MaxContext <= 0 {
		am.MaxContext = disc.MaxContext
	}
	if am.MaxOutput <= 0 {
		am.MaxOutput = disc.MaxOutput
	}
	return am
}

// resolveModelIntField resolves an integer field (max_context or max_output) for
// a model using the three-tier priority: agent model > catalog > registry.
func resolveModelIntField(m config.AgentModel, catalog *discovery.ModelCatalog,
	getAgent func(config.AgentModel) int,
	getCatalog func(discovery.DiscoveredModel) int,
	getRegistry func(config.ModelEntry) int) int {
	if v := getAgent(m); v > 0 {
		return v
	}
	if catalog != nil {
		if dm, ok := catalog.Lookup(m.Model); ok {
			if v := getCatalog(dm); v > 0 {
				return v
			}
		}
	}
	if entry, ok := config.ModelRegistry[m.Model]; ok {
		if v := getRegistry(entry); v > 0 {
			return v
		}
	}
	return 0
}

// resolveMaxContext returns the max context window for a model, checking
// agent model entry, then discovery catalog, then static registry.
func resolveMaxContext(m config.AgentModel, catalog *discovery.ModelCatalog) int {
	return resolveModelIntField(m, catalog,
		func(am config.AgentModel) int { return am.MaxContext },
		func(dm discovery.DiscoveredModel) int { return dm.MaxContext },
		func(re config.ModelEntry) int { return re.MaxContext })
}

// resolveMaxOutput returns the max output tokens for a model, checking
// agent model entry, then discovery catalog, then static registry.
func resolveMaxOutput(m config.AgentModel, catalog *discovery.ModelCatalog) int {
	return resolveModelIntField(m, catalog,
		func(am config.AgentModel) int { return am.MaxOutput },
		func(dm discovery.DiscoveredModel) int { return dm.MaxOutput },
		func(re config.ModelEntry) int { return re.MaxOutput })
}

func expandDeferredFromCatalog(m config.AgentModel, catalog *discovery.ModelCatalog, providers map[string]*config.Provider, logger *slog.Logger) ([]config.AgentModel, bool) {
	if catalog == nil {
		return nil, false
	}
	entries := catalog.LookupAll(m.Model)
	if len(entries) == 0 {
		return nil, false
	}
	expanded := make([]config.AgentModel, 0, len(entries))
	for _, e := range entries {
		if _, ok := providers[e.Provider]; !ok {
			continue
		}
		am := buildAgentModelWithFallback(m.Model, m, e)
		logger.Debug("expanded deferred provider",
			"model", m.Model, "provider", e.Provider,
			"max_context", am.MaxContext, "max_output", am.MaxOutput)
		expanded = append(expanded, am)
	}
	return expanded, true
}

func expandDeferredFromRegistry(m config.AgentModel, providers map[string]*config.Provider, logger *slog.Logger) (config.AgentModel, bool) {
	entry, ok := config.ModelRegistry[m.Model]
	if !ok {
		return config.AgentModel{}, false
	}
	p, ok := providers[entry.Provider]
	if !ok {
		return config.AgentModel{}, false
	}
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
	return am, true
}

func (a *AgentState) expandDeferredProviders(models []config.AgentModel, catalog *discovery.ModelCatalog, providers map[string]*config.Provider, logger *slog.Logger) []config.AgentModel {
	if len(models) >= math.MaxInt/2 {
		logger.Error("model count overflow in expandDeferredProviders", "count", len(models))
		return nil
	}
	expanded := make([]config.AgentModel, 0, len(models)*2)
	for _, m := range models {
		if m.Provider != "" || m.Model == "" {
			expanded = append(expanded, m)
			continue
		}

		if result, ok := expandDeferredFromCatalog(m, catalog, providers, logger); ok {
			expanded = append(expanded, result...)
			continue
		}

		if am, ok := expandDeferredFromRegistry(m, providers, logger); ok {
			expanded = append(expanded, am)
			continue
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

func expandWithCatalog(m config.AgentModel, providerName string, catalog *discovery.ModelCatalog, providers map[string]*config.Provider, logger *slog.Logger) ([]config.AgentModel, bool) {
	if catalog == nil {
		return nil, false
	}
	discModels := catalog.ModelsForProvider(providerName)
	if len(discModels) == 0 {
		return nil, false
	}
	expanded := make([]config.AgentModel, 0, len(discModels))
	for _, dm := range discModels {
		am := buildAgentModelWithFallback(dm.ID, m, dm)
		logger.Debug("expanded provider-only entry from catalog",
			"provider", providerName, "model", dm.ID,
			"max_context", am.MaxContext, "max_output", am.MaxOutput)
		expanded = append(expanded, am)
	}
	return expanded, true
}

func expandWithRegistryFallback(providerName string, m config.AgentModel, logger *slog.Logger) ([]config.AgentModel, bool) {
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
	if len(registryModels) == 0 {
		return nil, false
	}
	logger.Debug("expanded provider-only entry from ModelRegistry fallback",
		"provider", providerName, "count", len(registryModels))
	return registryModels, true
}

func (a *AgentState) expandProviderOnly(models []config.AgentModel, catalog *discovery.ModelCatalog, providers map[string]*config.Provider, logger *slog.Logger) []config.AgentModel {
	if len(models) >= math.MaxInt/2 {
		logger.Error("model count overflow in expandProviderOnly", "count", len(models))
		return nil
	}
	expanded := make([]config.AgentModel, 0, len(models)*2)
	for _, m := range models {
		if m.Provider == "" || m.Model != "" || m.ProviderRgx != "" || m.ModelRgx != "" {
			expanded = append(expanded, m)
			continue
		}

		providerName := m.Provider
		if _, ok := providers[providerName]; !ok {
			logger.Warn("provider-only entry: provider not configured", "provider", providerName)
			continue
		}

		if result, ok := expandWithCatalog(m, providerName, catalog, providers, logger); ok {
			expanded = append(expanded, result...)
			continue
		}

		if result, ok := expandWithRegistryFallback(providerName, m, logger); ok {
			expanded = append(expanded, result...)
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
			if _, ok := providers[dm.Provider]; !ok {
				logger.Debug("provider from catalog not found", "provider", dm.Provider, "model", dm.ID)
				continue
			}
			am := config.AgentModel{
				Provider:   dm.Provider,
				Model:      dm.ID,
				URL:        "",
				MaxContext: dm.MaxContext,
				MaxOutput:  dm.MaxOutput,
			}
			expanded = append(expanded, am)
		}
		if !matchedAny {
			registryModels := util.FindRegistryModels(m, providers)
			expanded = append(expanded, registryModels...)
			if len(registryModels) > 0 {
				matchedAny = true
			}
		}
		if !matchedAny {
			logger.Warn("dynamic model entry matched no catalog or registry entries",
				"provider_rgx", m.ProviderRgx, "model_rgx", m.ModelRgx)
		}
	}
	return expanded
}

func (a *AgentState) selectStart(agent config.AgentConfig, agentName string, n int) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	if agent.Strategy == "fallback" {
		return 0
	}

	start := int(a.Counters[agentName]) % n
	a.Counters[agentName]++
	return start
}

func resolveTargetFormat(model string, agentModel *config.AgentModel, catalog *discovery.ModelCatalog) string {
	if agentModel != nil && agentModel.Format != "" {
		return agentModel.Format
	}
	if catalog != nil {
		if dm, ok := catalog.Lookup(model); ok {
			return dm.Format
		}
	}
	if entry, ok := config.ModelRegistry[model]; ok {
		return entry.Format
	}
	if catalog != nil && model != "" {
		return discovery.InferFormat(model)
	}
	return ""
}

func (a *AgentState) buildTarget(logger *slog.Logger, agentName string, m config.AgentModel, tokenCount int, providers map[string]*config.Provider, catalog *discovery.ModelCatalog, autoContextSkip bool) (*UpstreamTarget, bool) {
	maxCtx := resolveMaxContext(m, catalog)
	if maxCtx > 0 && tokenCount > maxCtx {
		logger.Info("skipping model: exceeds max_context",
			"provider", m.Provider, "model", m.Model, "max_context", maxCtx, "tokens", tokenCount)
		return nil, false
	}

	if autoContextSkip && maxCtx > 0 && tokenCount > 0 && maxCtx < tokenCount*2 {
		logger.Info("skipping model: context headroom too small",
			"provider", m.Provider, "model", m.Model, "max_context", maxCtx, "tokens", tokenCount)
		return nil, false
	}

	if !checkCapabilities(m, catalog) {
		return nil, false
	}

	if m.Provider != "" {
		provider := providers[m.Provider]
		if provider == nil || (provider.APIKey == "" && provider.AuthStyle != "none") {
			logger.Debug("provider has no API key, skipping model", "provider", m.Provider, "model", m.Model)
			return nil, false
		}
	}

	maxOut := resolveMaxOutput(m, catalog)

	formatURLs := map[string]string{}
	if m.Provider != "" {
		if provider := providers[m.Provider]; provider != nil {
			formatURLs = provider.FormatURLs
		}
	}
	p := ProviderURL(m.Provider, m.URL, m.Format, formatURLs, providers)
	if p == "" {
		logger.Warn("unknown provider, skipping model", "provider", m.Provider, "model", m.Model)
		return nil, false
	}

	return &UpstreamTarget{
		URL:        p,
		Model:      m.Model,
		Format:     resolveTargetFormat(m.Model, &m, catalog),
		CoolKey:    agentName + ":" + m.Provider + ":" + m.Model,
		Provider:   m.Provider,
		MaxOutput:  maxOut,
		MaxContext: maxCtx,
	}, true
}

// ActivateCooldown forces the circuit breaker for a target into the open state
// for the specified cooldown duration.
func (a *AgentState) ActivateCooldown(target UpstreamTarget, cooldownDuration time.Duration) {
	a.CB.ForceOpen(target.CoolKey, cooldownDuration)
}

// RecordFailure records a failure for a target's circuit breaker,
// potentially opening the circuit after the threshold is reached.
func (a *AgentState) RecordFailure(target UpstreamTarget, cooldownDuration time.Duration) {
	if target.CoolKey == "" {
		return
	}
	a.CB.RecordFailure(target.CoolKey, cooldownDuration)
}

// RecordSuccess records a successful request for the given circuit breaker key,
// potentially transitioning the circuit to closed state.
func (a *AgentState) RecordSuccess(key string) {
	a.CB.RecordSuccess(key)
}

// ActiveCooldowns returns the number of currently active cooldowns.
func (a *AgentState) ActiveCooldowns() int {
	return a.CB.ActiveCount()
}

// CBSnapshot returns a snapshot of all circuit breaker states as a map
// from key to state name.
func (a *AgentState) CBSnapshot() map[string]string {
	return a.CB.Snapshot()
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

// ResolveWindowMaxContext returns the maximum context window across all models
// configured for a given agent name. Returns 0 if the agent is not found.
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

// SortTargetsByLatency orders targets by median latency using the provided
// latency tracker. If jitterFn is non-nil, it applies randomization (±5%)
// to prevent thundering herd. Returns the original slice if latencyTracker
// is nil or the slice has ≤1 element.
func SortTargetsByLatency(targets []UpstreamTarget, lt *infra.LatencyTracker, jitterFn func() float64) []UpstreamTarget {
	if lt == nil || len(targets) <= 1 {
		return targets
	}

	keys := make([]infra.LatencyKey, len(targets))
	for i, t := range targets {
		keys[i] = infra.LatencyKey{Model: t.Model, Provider: t.Provider}
	}

	indices := lt.SortByLatency(keys, jitterFn)

	sorted := make([]UpstreamTarget, len(targets))
	for i, idx := range indices {
		sorted[i] = targets[idx]
	}
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
