package routing

import (
	"log/slog"
	"sync"
	"time"

	"nenya/internal/config"
)

type AgentState struct {
	Counters     map[string]uint64
	ModelCooldowns map[string]time.Time
	Mu           sync.Mutex
}

func NewAgentState() *AgentState {
	return &AgentState{
		Counters:      make(map[string]uint64),
		ModelCooldowns: make(map[string]time.Time),
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
	now := time.Now()
	cooldowns := make(map[string]time.Time, len(a.ModelCooldowns))
	for k, v := range a.ModelCooldowns {
		if v.After(now) {
			cooldowns[k] = v
		} else {
			delete(a.ModelCooldowns, k)
		}
	}
	a.ModelCooldowns = cooldowns
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
		if now.Before(cooldowns[t.CoolKey]) {
			cooling = append(cooling, t)
		} else {
			active = append(active, t)
		}
	}
	return append(active, cooling...)
}

func (a *AgentState) ActivateCooldown(target UpstreamTarget, cooldownDuration time.Duration) {
	if target.CoolKey == "" || cooldownDuration == 0 {
		return
	}
	a.Mu.Lock()
	a.ModelCooldowns[target.CoolKey] = time.Now().Add(cooldownDuration)
	a.Mu.Unlock()
}

func (a *AgentState) ActiveCooldowns() int {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	now := time.Now()
	count := 0
	for _, expiry := range a.ModelCooldowns {
		if expiry.After(now) {
			count++
		}
	}
	return count
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
