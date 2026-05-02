package discovery

import (
	"log/slog"
	"sort"
	"strings"

	"nenya/config"
)

const (
	AgentStrategyRoundRobin = "round-robin"
	AgentStrategyFallback   = "fallback"
)

type AutoAgentConfig struct {
	Name        string
	Description string
	Strategy    string
	Filter      func(DiscoveredModel) bool
}

func GenerateAutoAgents(catalog *ModelCatalog, providersMap map[string]*config.Provider, cfg *config.AutoAgentsConfig, logger *slog.Logger) map[string]config.AgentConfig {
	agents := make(map[string]config.AgentConfig)

	logger.Debug("auto-agents generation started",
		"catalog_models", len(catalog.AllModels()),
		"providers", len(providersMap),
		"config_provided", cfg != nil,
	)

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
		{
			Name:        "auto_coding",
			Description: "Code-optimized models with tool calling capability",
			Strategy:    AgentStrategyFallback,
			Filter:      isCodingModel,
		},
	}

	for _, agentCfg := range agentConfigs {
		category := strings.TrimPrefix(agentCfg.Name, "auto_")
		if !cfg.IsEnabled(category) {
			logger.Debug("auto-agent disabled by config", "agent", agentCfg.Name)
			continue
		}

		models := filterModels(catalog, providersMap, agentCfg.Filter, logger)
		if len(models) == 0 {
			logger.Debug("auto-agent has no models, skipping", "agent", agentCfg.Name)
			continue
		}

		agents[agentCfg.Name] = config.AgentConfig{
			Strategy: agentCfg.Strategy,
			Models:   models,
		}

		modelIDs := make([]string, 0, len(models))
		for _, m := range models {
			modelIDs = append(modelIDs, m.Model)
		}

		logger.Debug("generated auto-agent",
			"agent", agentCfg.Name,
			"description", agentCfg.Description,
			"strategy", agentCfg.Strategy,
			"models", modelIDs,
		)
	}

	var agentNames []string
	for name := range agents {
		agentNames = append(agentNames, name)
	}
	sort.Strings(agentNames)
	logger.Debug("auto-agents summary",
		"total_agents", len(agents),
		"agents", agentNames,
	)

	logger.Debug("auto-agents generation completed", "generated_count", len(agents))

	return agents
}

func filterModels(catalog *ModelCatalog, providersMap map[string]*config.Provider, filter func(DiscoveredModel) bool, logger *slog.Logger) []config.AgentModel {
	var models []config.AgentModel

	for _, m := range catalog.AllModels() {
		provider, ok := providersMap[m.Provider]
		if !ok {
			continue
		}

		if provider.APIKey == "" && provider.AuthStyle != "none" {
			continue
		}

		if filter(m) {
			models = append(models, config.AgentModel{
				Model:    m.ID,
				Provider: m.Provider,
			})
		}
	}

	return models
}

func isFastModel(m DiscoveredModel) bool {
	return m.MaxContext > 0 && m.MaxContext <= 32000 &&
		m.MaxOutput > 0 && m.MaxOutput <= 4096
}

func isReasoningModel(m DiscoveredModel) bool {
	return m.HasCapability("reasoning") && m.MaxContext >= 128000
}

func isVisionModel(m DiscoveredModel) bool {
	return m.HasCapability("vision")
}

func isToolModel(m DiscoveredModel) bool {
	return m.HasCapability("tool_calls")
}

func isLargeModel(m DiscoveredModel) bool {
	return m.MaxContext >= 200000
}

func isBalancedModel(m DiscoveredModel) bool {
	return m.MaxContext > 32000 && m.MaxContext < 128000
}

var codingPrefixes = []string{
	"codestral", "devstral", "deepseek-v4", "deepseek-r1",
	"qwen2.5", "qwen3", "code-llama", "phi-4",
	"claude-sonnet", "claude-3-5", "claude-3-7",
	"gemini-2.5", "gemini-3",
	"grok-3", "grok-4",
	"mistral-large", "glm-5",
}

func isCodingModel(m DiscoveredModel) bool {
	if !m.HasCapability("tool_calls") {
		return false
	}
	id := strings.ToLower(m.ID)
	for _, prefix := range codingPrefixes {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}
