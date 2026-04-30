package routing

import (
	"strings"

	"nenya/internal/config"
	"nenya/internal/discovery"
)

type UpstreamTarget struct {
	URL        string
	Model      string
	Format     string
	CoolKey    string
	Provider   string
	MaxOutput  int
	MaxContext int
}

type ProviderMatch struct {
	Provider   string
	Model      string
	Format     string
	MaxContext int
	MaxOutput  int
}

func ResolveProviders(modelName string, providers map[string]*config.Provider, catalog *discovery.ModelCatalog) []ProviderMatch {
	if providers == nil {
		return nil
	}

	if catalog != nil {
		if matches := resolveFromCatalog(modelName, providers, catalog); len(matches) > 0 {
			return matches
		}
	}

	return resolveFromRegistry(modelName, providers)
}

func resolveFromCatalog(modelName string, providers map[string]*config.Provider, catalog *discovery.ModelCatalog) []ProviderMatch {
	entries := catalog.LookupAll(modelName)
	if len(entries) == 0 {
		return nil
	}
	matches := make([]ProviderMatch, 0, len(entries))
	for _, e := range entries {
		p, ok := providers[e.Provider]
		if !ok {
			continue
		}
		if p.APIKey == "" && p.AuthStyle != "none" {
			continue
		}
		matches = append(matches, ProviderMatch{
			Provider:   e.Provider,
			Model:      modelName,
			Format:     e.Format,
			MaxContext: e.MaxContext,
			MaxOutput:  e.MaxOutput,
		})
	}
	return matches
}

func resolveFromRegistry(modelName string, providers map[string]*config.Provider) []ProviderMatch {
	entry, ok := config.ModelRegistry[strings.ToLower(modelName)]
	if !ok {
		return nil
	}
	p, ok := providers[entry.Provider]
	if !ok {
		return nil
	}
	if p.APIKey == "" && p.AuthStyle != "none" {
		return nil
	}
	return []ProviderMatch{{
		Provider:   p.Name,
		Model:      modelName,
		Format:     entry.Format,
		MaxContext: entry.MaxContext,
		MaxOutput:  entry.MaxOutput,
	}}
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

func ProviderURL(provider, agentURL, format string, formatURLs map[string]string, providers map[string]*config.Provider) string {
	if agentURL != "" {
		return agentURL
	}
	if format != "" && formatURLs != nil {
		if u, ok := formatURLs[format]; ok {
			return u
		}
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
