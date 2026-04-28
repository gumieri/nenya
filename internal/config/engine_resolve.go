package config

import (
	"fmt"
)

// ResolveEngineRef resolves an engine reference to a list of concrete targets.
// It handles both inline engine configurations and agent-based references.
func ResolveEngineRef(ref EngineRef, agents map[string]AgentConfig, providers map[string]*Provider) ([]EngineTarget, error) {
	if ref.AgentName != "" {
		return resolveAgentEngineRef(ref, agents, providers)
	}
	return resolveInlineEngineRef(ref, providers)
}

func resolveAgentEngineRef(ref EngineRef, agents map[string]AgentConfig, providers map[string]*Provider) ([]EngineTarget, error) {
	agent, ok := agents[ref.AgentName]
	if !ok {
		return nil, fmt.Errorf("engine agent %q not found", ref.AgentName)
	}
	if len(agent.Models) == 0 {
		return nil, fmt.Errorf("engine agent %q has no models defined", ref.AgentName)
	}

	targets := make([]EngineTarget, 0, len(agent.Models))
	for _, m := range agent.Models {
		target, err := buildAgentEngineTarget(ref, m, agent, providers)
		if err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func buildAgentEngineTarget(ref EngineRef, m AgentModel, agent AgentConfig, providers map[string]*Provider) (EngineTarget, error) {
	providerURL := getProviderURL(m.Provider, m.URL, providers)
	if providerURL == "" {
		return EngineTarget{}, fmt.Errorf("engine agent %q: provider %q not found and no URL specified", ref.AgentName, m.Provider)
	}

	providerTimeout := getProviderTimeout(m.Provider, providers)
	systemPrompt, systemPromptFile := resolveSystemPrompts(ref, agent)
	timeoutSeconds := resolveTimeout(ref, providerTimeout)

	return EngineTarget{
		Engine: EngineConfig{
			Provider:         m.Provider,
			Model:            m.Model,
			SystemPrompt:     systemPrompt,
			SystemPromptFile: systemPromptFile,
			TimeoutSeconds:   timeoutSeconds,
		},
		Provider: buildProvider(m.Provider, providerURL, providerTimeout, providers),
	}, nil
}

func resolveInlineEngineRef(ref EngineRef, providers map[string]*Provider) ([]EngineTarget, error) {
	providerURL, providerTimeout := getInlineProviderConfig(ref, providers)
	if providerURL == "" {
		return nil, fmt.Errorf("engine provider %q not found", ref.Provider)
	}

	timeoutSeconds := ref.TimeoutSeconds
	if timeoutSeconds == 0 && providerTimeout > 0 {
		timeoutSeconds = providerTimeout
	}

	return []EngineTarget{
		{
			Engine: EngineConfig{
				Provider:         ref.Provider,
				Model:            ref.Model,
				SystemPrompt:     ref.SystemPrompt,
				SystemPromptFile: ref.SystemPromptFile,
				TimeoutSeconds:   timeoutSeconds,
			},
			Provider: buildProvider(ref.Provider, providerURL, providerTimeout, providers),
		},
	}, nil
}

func getProviderURL(providerName string, modelURL string, providers map[string]*Provider) string {
	if modelURL != "" {
		return modelURL
	}
	if p, ok := providers[providerName]; ok {
		return p.URL
	}
	return ""
}

func getProviderTimeout(providerName string, providers map[string]*Provider) int {
	if p, ok := providers[providerName]; ok {
		return p.TimeoutSeconds
	}
	return 0
}

func resolveSystemPrompts(ref EngineRef, agent AgentConfig) (string, string) {
	if ref.SystemPrompt != "" || ref.SystemPromptFile != "" {
		return ref.SystemPrompt, ref.SystemPromptFile
	}
	return agent.SystemPrompt, agent.SystemPromptFile
}

func resolveTimeout(ref EngineRef, providerTimeout int) int {
	if ref.TimeoutSeconds != 0 {
		return ref.TimeoutSeconds
	}
	if providerTimeout > 0 {
		return providerTimeout
	}
	return 0
}

func getInlineProviderConfig(ref EngineRef, providers map[string]*Provider) (string, int) {
	if ref.Provider == "" {
		return "", 0
	}
	if p, ok := providers[ref.Provider]; ok {
		return p.URL, p.TimeoutSeconds
	}
	return "", 0
}

func buildProvider(providerName string, url string, timeout int, providers map[string]*Provider) *Provider {
	return &Provider{
		Name:           providerName,
		URL:            url,
		APIKey:         "",
		TimeoutSeconds: timeout,
		AuthStyle:      getProviderAuthStyle(providerName, providers),
		ApiFormat:      getProviderApiFormat(providerName, providers),
	}
}

func getProviderAuthStyle(providerName string, providers map[string]*Provider) string {
	if p, ok := providers[providerName]; ok {
		return p.AuthStyle
	}
	return ""
}

func getProviderApiFormat(providerName string, providers map[string]*Provider) string {
	if p, ok := providers[providerName]; ok {
		return p.ApiFormat
	}
	return ""
}

func resolveEngineRefs(cfg *Config) error {
	providers := make(map[string]*Provider)
	for name, pc := range cfg.Providers {
		providers[name] = &Provider{
			Name:                 name,
			URL:                  pc.URL,
			RoutePrefixes:        pc.RoutePrefixes,
			AuthStyle:            pc.AuthStyle,
			ApiFormat:            pc.ApiFormat,
			TimeoutSeconds:       pc.TimeoutSeconds,
			RetryableStatusCodes: pc.RetryableStatusCodes,
		}
	}

	if err := resolveSingleEngineRef(&cfg.SecurityFilter.Engine, cfg.Agents, providers, "security_filter"); err != nil {
		return err
	}
	if err := resolveSingleEngineRef(&cfg.Window.Engine, cfg.Agents, providers, "window"); err != nil {
		return err
	}

	return nil
}

func resolveSingleEngineRef(ref *EngineRef, agents map[string]AgentConfig, providers map[string]*Provider, label string) error {
	targets, err := ResolveEngineRef(*ref, agents, providers)
	if err != nil {
		return fmt.Errorf("%s engine: %w", label, err)
	}
	ref.ResolvedTargets = targets
	return nil
}
