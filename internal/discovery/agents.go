package discovery

import (
	"log/slog"

	"nenya/internal/config"
	"nenya/internal/providers"
)

const (
	AgentStrategyRoundRobin = "round-robin"
	AgentStrategyFallback   = "fallback"
)

type AutoAgentConfig struct {
	Name        string
	Description string
	Strategy    string
	Filter      func(DiscoveredModel, providers.ProviderSpec) bool
}

func GenerateAutoAgents(catalog *ModelCatalog, providersMap map[string]*config.Provider, logger *slog.Logger) map[string]config.AgentConfig {
	agents := make(map[string]config.AgentConfig)

	agentConfigs := []AutoAgentConfig{
		{
			Name:        "auto_fast",
			Description: "Fast models with small context windows (≤32k context, ≤4k output)",
			Strategy:    AgentStrategyRoundRobin,
			Filter:      isFastModel,
		},
		{
			Name:        "auto_reasoning",
			Description: "Reasoning models with large context windows (≥128k context, supports reasoning)",
			Strategy:    AgentStrategyFallback,
			Filter:      isReasoningModel,
		},
		{
			Name:        "auto_vision",
			Description: "Vision-capable models for image analysis",
			Strategy:    AgentStrategyRoundRobin,
			Filter:      isVisionModel,
		},
		{
			Name:        "auto_tools",
			Description: "Tool-capable models for function calling",
			Strategy:    AgentStrategyRoundRobin,
			Filter:      isToolModel,
		},
		{
			Name:        "auto_large",
			Description: "Large context window models (≥200k context)",
			Strategy:    AgentStrategyFallback,
			Filter:      isLargeModel,
		},
		{
			Name:        "auto_balanced",
			Description: "Balanced models for general purpose (32k < context < 128k)",
			Strategy:    AgentStrategyRoundRobin,
			Filter:      isBalancedModel,
		},
	}

	for _, agentCfg := range agentConfigs {
		models := filterModels(catalog, providersMap, agentCfg.Filter, logger)
		if len(models) == 0 {
			logger.Debug("auto-agent has no models, skipping", "agent", agentCfg.Name)
			continue
		}

		agents[agentCfg.Name] = config.AgentConfig{
			Strategy: agentCfg.Strategy,
			Models:   models,
		}

		logger.Info("generated auto-agent", "agent", agentCfg.Name, "description", agentCfg.Description, "models", len(models))
	}

	return agents
}

func filterModels(catalog *ModelCatalog, providersMap map[string]*config.Provider, filter func(DiscoveredModel, providers.ProviderSpec) bool, logger *slog.Logger) []config.AgentModel {
	var models []config.AgentModel

	for _, m := range catalog.AllModels() {
		provider, ok := providersMap[m.Provider]
		if !ok {
			continue
		}

		if provider.APIKey == "" && provider.AuthStyle != "none" {
			continue
		}

		spec, ok := providers.Get(m.Provider)
		if !ok {
			continue
		}

		if filter(m, spec) {
			models = append(models, config.AgentModel{
				Model:    m.ID,
				Provider: m.Provider,
			})
		}
	}

	return models
}

func isFastModel(m DiscoveredModel, spec providers.ProviderSpec) bool {
	return m.MaxContext > 0 && m.MaxContext <= 32000 &&
		m.MaxOutput > 0 && m.MaxOutput <= 4096
}

func isReasoningModel(m DiscoveredModel, spec providers.ProviderSpec) bool {
	return m.MaxContext >= 128000 && spec.SupportsReasoning
}

func isVisionModel(m DiscoveredModel, spec providers.ProviderSpec) bool {
	return spec.SupportsVision
}

func isToolModel(m DiscoveredModel, spec providers.ProviderSpec) bool {
	return spec.SupportsToolCalls
}

func isLargeModel(m DiscoveredModel, spec providers.ProviderSpec) bool {
	return m.MaxContext >= 200000
}

func isBalancedModel(m DiscoveredModel, spec providers.ProviderSpec) bool {
	return m.MaxContext > 32000 && m.MaxContext < 128000
}
