package routing

import (
	"strings"

	"nenya/internal/config"
)

type UpstreamTarget struct {
	URL       string
	Model     string
	CoolKey   string
	Provider  string
	MaxOutput int
}

func ResolveProvider(modelName string, providers map[string]*config.Provider) *config.Provider {
	if entry, ok := config.ModelRegistry[modelName]; ok {
		if p, ok := providers[entry.Provider]; ok {
			return p
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
	if p := ResolveProvider(modelName, providers); p != nil {
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
