package routing

import (
	"log/slog"
	"math/rand"
	"sort"
	"sync"
	"time"

	"nenya/internal/config"
	"nenya/internal/discovery"
	"nenya/internal/infra"
	"nenya/internal/resilience"
)

const (
	DefaultAgentCooldownSec    = 60
	DefaultFailureThreshold    = 5
	DefaultSuccessThreshold    = 1
	DefaultHalfOpenMaxRequests = 3
	latencyJitterPct           = 0.10
)

type AgentState struct {
	Counters map[string]uint64
	CB       *resilience.CircuitBreaker
	Mu       sync.Mutex
}

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
		Counters: make(map[string]uint64),
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
	n := len(agent.Models)
	if n == 0 {
		return nil
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
		m := agent.Models[(start+i)%n]

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
