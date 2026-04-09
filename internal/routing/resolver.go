package routing

import (
	"strings"

	"nenya/internal/config"
)

var GeminiModelMap = map[string]string{
	"gemini-3-flash":        "gemini-3-flash-preview",
	"gemini-3-pro":          "gemini-3-pro-preview",
	"gemini-3.1-flash":      "gemini-3.1-flash-preview",
	"gemini-3.1-flash-lite": "gemini-3.1-flash-lite-preview",
	"gemini-3.1-pro":        "gemini-3.1-pro-preview",
	"gemini-flash":          "gemini-2.5-flash",
	"gemini-flash-lite":     "gemini-2.5-flash-lite",
	"gemini-pro":            "gemini-2.5-pro",
}

type UpstreamTarget struct {
	URL       string
	Model     string
	CoolKey   string
	Provider  string
	MaxOutput int
}

const DefaultAgentCooldownSec = 60

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
	if defaultP, ok := providers["zai"]; ok {
		return defaultP.URL
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

func IsGeminiProvider(providerName string, providers map[string]*config.Provider) bool {
	if p, ok := providers[providerName]; ok {
		return p.AuthStyle == "bearer+x-goog"
	}
	return false
}

func IsZAIProvider(providerName string) bool {
	return providerName == "zai"
}
