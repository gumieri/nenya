package routing

import (
	"strings"

	"nenya/internal/config"
	"nenya/internal/discovery"
)

// UpstreamTarget describes a resolved target for forwarding a request,
// including the provider URL, model name, cooldown key, and context limits.
type UpstreamTarget struct {
	URL        string
	Model      string
	CoolKey    string
	Provider   string
	MaxOutput  int
	MaxContext int
}

// ResolveProvider looks up the provider for the given model name, checking
// the static ModelRegistry first, then the dynamic ModelCatalog, then
// route_prefixes. Returns nil if no provider matches.
func ResolveProvider(modelName string, providers map[string]*config.Provider, catalog *discovery.ModelCatalog) *config.Provider {
	if entry, ok := config.ModelRegistry[modelName]; ok {
		if p, ok := providers[entry.Provider]; ok {
			return p
		}
	}

	if catalog != nil {
		if m, ok := catalog.Lookup(modelName); ok {
			if p, ok := providers[m.Provider]; ok {
				return p
			}
		}
	}

	lower := strings.ToLower(modelName)
	for _, p := range providers {
		for _, prefix := range p.RoutePrefixes {
			if strings.HasPrefix(lower, prefix) {
				return p
			}
		}
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
		if m, ok := catalog.Lookup(modelID); ok {
			return m.MaxContext, m.MaxOutput
		}
	}
	if entry, ok := config.ModelRegistry[modelID]; ok {
		return entry.MaxContext, entry.MaxOutput
	}
	return 0, 0
}
