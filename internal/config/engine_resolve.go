package config

import (
	"fmt"
)

func ResolveEngineRef(ref EngineRef, agents map[string]AgentConfig, providers map[string]*Provider) ([]EngineTarget, error) {
	if ref.AgentName != "" {
		agent, ok := agents[ref.AgentName]
		if !ok {
			return nil, fmt.Errorf("engine agent %q not found", ref.AgentName)
		}
		if len(agent.Models) == 0 {
			return nil, fmt.Errorf("engine agent %q has no models defined", ref.AgentName)
		}

		targets := make([]EngineTarget, 0, len(agent.Models))
		for _, m := range agent.Models {
			providerURL := ""
			if m.URL != "" {
				providerURL = m.URL
			} else if p, ok := providers[m.Provider]; ok {
				providerURL = p.URL
			}
			if providerURL == "" {
				return nil, fmt.Errorf("engine agent %q: provider %q not found and no URL specified", ref.AgentName, m.Provider)
			}

			systemPrompt := ref.SystemPrompt
			systemPromptFile := ref.SystemPromptFile
			if systemPrompt == "" && systemPromptFile == "" {
				systemPrompt = agent.SystemPrompt
				systemPromptFile = agent.SystemPromptFile
			}

			target := EngineTarget{
				Engine: EngineConfig{
					Provider:         m.Provider,
					Model:            m.Model,
					SystemPrompt:     systemPrompt,
					SystemPromptFile: systemPromptFile,
					TimeoutSeconds:   ref.TimeoutSeconds,
				},
				Provider: &Provider{
					Name:   m.Provider,
					URL:    providerURL,
					APIKey: "",
					AuthStyle: func() string {
						if p, ok := providers[m.Provider]; ok {
							return p.AuthStyle
						}
						return ""
					}(),
					ApiFormat: func() string {
						if p, ok := providers[m.Provider]; ok {
							return p.ApiFormat
						}
						return ""
					}(),
				},
			}
			targets = append(targets, target)
		}
		return targets, nil
	}

	providerURL := ""
	if ref.Provider != "" {
		if p, ok := providers[ref.Provider]; ok {
			providerURL = p.URL
		}
	}
	if providerURL == "" {
		return nil, fmt.Errorf("engine provider %q not found", ref.Provider)
	}

	return []EngineTarget{
		{
			Engine: EngineConfig{
				Provider:         ref.Provider,
				Model:            ref.Model,
				SystemPrompt:     ref.SystemPrompt,
				SystemPromptFile: ref.SystemPromptFile,
				TimeoutSeconds:   ref.TimeoutSeconds,
			},
			Provider: &Provider{
				Name:   ref.Provider,
				URL:    providerURL,
				APIKey: "",
				AuthStyle: func() string {
					if p, ok := providers[ref.Provider]; ok {
						return p.AuthStyle
					}
					return ""
				}(),
				ApiFormat: func() string {
					if p, ok := providers[ref.Provider]; ok {
						return p.ApiFormat
					}
					return ""
				}(),
			},
		},
	}, nil
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
