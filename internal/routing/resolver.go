package routing

import (
	"strings"

	"nenya/internal/config"
	"nenya/internal/discovery"
)

type UpstreamTarget struct {
	URL        string
	Model      string
	CoolKey    string
	Provider   string
	MaxOutput  int
	MaxContext int
}

type ProviderMatch struct {
	Provider   string
	Model      string
	MaxContext int
	MaxOutput  int
}

func ResolveProviders(modelName string, providers map[string]*config.Provider, catalog *discovery.ModelCatalog) []ProviderMatch {
	if providers == nil {
		return nil
	}

	if catalog != nil {
		entries := catalog.LookupAll(modelName)
		if len(entries) > 0 {
			matches := make([]ProviderMatch, 0, len(entries))
			for _, e := range entries {
				if _, ok := providers[e.Provider]; ok {
					matches = append(matches, ProviderMatch{
						Provider:   e.Provider,
						Model:      modelName,
						MaxContext: e.MaxContext,
						MaxOutput:  e.MaxOutput,
					})
				}
			}
			if len(matches) > 0 {
				return matches
			}
		}
	}

	if entry, ok := config.ModelRegistry[strings.ToLower(modelName)]; ok {
		if p, ok := providers[entry.Provider]; ok {
			return []ProviderMatch{{
				Provider:   p.Name,
				Model:      modelName,
				MaxContext: entry.MaxContext,
				MaxOutput:  entry.MaxOutput,
			}}
		}
	}

	if entry, ok := config.ModelRegistry[modelName]; ok {
		if p, ok := providers[entry.Provider]; ok {
			return []ProviderMatch{{
				Provider:   p.Name,
				Model:      modelName,
				MaxContext: entry.MaxContext,
				MaxOutput:  entry.MaxOutput,
			}}
		}
	}

	return nil
}

func ResolveProvider(modelName string, providers map[string]*config.Provider, catalog *discovery.ModelCatalog) *config.Provider {
	matches := ResolveProviders(modelName, providers, catalog)
	if len(matches) == 0 {
		return nil
	}
	if p, ok := providers[matches[0].Provider]; ok {
		return p
	}
	return nil
}

func DetermineUpstream(modelName string, providers map[string]*config.Provider) string {
	if p := ResolveProvider(modelName, providers, nil); p != nil {
		return p.URL
	}
	return ""
}

func ProviderURL(provider, agentURL string, providers map[string]*config.Provider) string {
	if agentURL != "" {
		return agentURL
	}
	if p, ok := providers[provider]; ok {
		return p.URL
	}
	return ""
}

func ResolveModelLimits(modelID string, catalog *discovery.ModelCatalog) (maxContext, maxOutput int) {
	if catalog != nil {
		entries := catalog.LookupAll(modelID)
		if len(entries) > 0 {
			return entries[0].MaxContext, entries[0].MaxOutput
		}
	}
	if entry, ok := config.ModelRegistry[modelID]; ok {
		return entry.MaxContext, entry.MaxOutput
	}
	return 0, 0
}
