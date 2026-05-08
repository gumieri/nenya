package routing

import (
	"strings"

	"nenya/config"
	"nenya/internal/discovery"
)

// UpstreamTarget represents a concrete routing target with all necessary
// fields to forward a request to a specific provider/model combination.
type UpstreamTarget struct {
	URL        string
	Model      string
	Format     string
	CoolKey    string
	Provider   string
	MaxOutput  int
	MaxContext int
}

// ProviderMatch holds the resolved provider details for a model,
// including format, context limits, and the provider name.
type ProviderMatch struct {
	Provider   string
	Model      string
	Format     string
	MaxContext int
	MaxOutput  int
}

// ResolveProviders returns all matching providers for a model name,
// checking the dynamic discovery catalog first, then falling back to the
// static ModelRegistry. Returns an empty slice for unknown models.
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

// ResolveProvider resolves the first provider for a model name,
// checking the dynamic discovery catalog first, then falling back to the
// static ModelRegistry. Returns nil for unknown models.
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

// DetermineUpstream returns the base endpoint URL for a model's provider.
// Returns an empty string if the model cannot be resolved.
func DetermineUpstream(modelName string, providers map[string]*config.Provider) string {
	if p := ResolveProvider(modelName, providers, nil); p != nil {
		return p.URL
	}
	return ""
}

// ProviderURL selects the final endpoint URL for a given provider/model/format
// combination. Priority: agent URL > format-specific URL > provider default URL.
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

// ResolveModelLimits returns the max_context and max_output for a model,
// checking the dynamic discovery catalog first, then falling back to the
// static ModelRegistry.
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
