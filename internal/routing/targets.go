package routing

import (
	"log/slog"
	"sync"
	"time"

	"nenya/internal/config"
	"nenya/internal/resilience"
)

const (
	DefaultAgentCooldownSec = 60
	DefaultFailureThreshold = 5
	DefaultFailureWindowSec = 60
	DefaultSuccessThreshold = 1
)

type AgentState struct {
	Counters map[string]uint64
	CB       *resilience.CircuitBreaker
	Mu       sync.Mutex
}

func NewAgentState() *AgentState {
	return &AgentState{
		Counters: make(map[string]uint64),
		CB: resilience.NewCircuitBreaker(
			DefaultFailureThreshold,
			DefaultSuccessThreshold,
			time.Duration(DefaultFailureWindowSec)*time.Second,
			time.Duration(DefaultAgentCooldownSec)*time.Second,
		),
	}
}

func (a *AgentState) BuildTargetList(logger *slog.Logger, agentName string, agent config.AgentConfig, tokenCount int, providers map[string]*config.Provider) []UpstreamTarget {
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
			if entry, ok := config.ModelRegistry[m.Model]; ok && entry.MaxContext > 0 {
				maxCtx = entry.MaxContext
			}
		}
		if maxCtx > 0 && tokenCount > maxCtx {
			logger.Info("skipping model: exceeds max_context",
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
			if entry, ok := config.ModelRegistry[m.Model]; ok && entry.MaxOutput > 0 {
				maxOut = entry.MaxOutput
			}
		}

		t := UpstreamTarget{
			URL:       p,
			Model:     m.Model,
			CoolKey:   agentName + ":" + m.Provider + ":" + m.Model,
			Provider:  m.Provider,
			MaxOutput: maxOut,
		}
		if a.CB.Allow(t.CoolKey) {
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

func (a *AgentState) RecordFailure(target UpstreamTarget, cooldownDuration time.Duration) bool {
	if target.CoolKey == "" {
		return false
	}
	return a.CB.RecordFailure(target.CoolKey, cooldownDuration)
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

func ResolveWindowMaxContext(modelName string, agents map[string]config.AgentConfig) int {
	if agent, ok := agents[modelName]; ok {
		maxCtx := 0
		for _, m := range agent.Models {
			mc := m.MaxContext
			if mc == 0 {
				if entry, ok := config.ModelRegistry[m.Model]; ok && entry.MaxContext > 0 {
					mc = entry.MaxContext
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
