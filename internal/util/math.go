package util

import (
	"fmt"
	"math"
	"strings"

	"nenya/config"
)

// AddCap returns a+b, clamped to math.MaxInt on overflow.
// Use this for slice capacity calculations where a+b could exceed
// maximum int value.
func AddCap(a, b int) int {
	if b > 0 && a > math.MaxInt-b {
		return math.MaxInt
	}
	return a + b
}

// JoinBackticks formats a slice of names as a comma-separated list
// wrapped in backticks. For example, ["foo", "bar"] becomes "`foo`, `bar`".
func JoinBackticks(names []string) string {
	var sb strings.Builder
	for i, name := range names {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteByte('`')
		sb.WriteString(name)
		sb.WriteByte('`')
	}
	return sb.String()
}

// ErrNoProvider is the error message returned when no provider can be
// resolved for a given model name.
const ErrNoProvider = "No provider configured for this model"

// ErrNoProviderFmt returns ErrNoProvider formatted with the model name.
func ErrNoProviderFmt(model string) string {
	return fmt.Sprintf("%s: %s", ErrNoProvider, model)
}

// ProviderCanServe returns true if the provider is configured with either
// an API key or auth_style "none" (i.e. can actually make upstream requests).
func ProviderCanServe(p *config.Provider) bool {
	return p != nil && (p.APIKey != "" || p.AuthStyle == "none")
}

// FindRegistryModels returns models from ModelRegistry matching the given pattern.
// The pattern can be a literal model string, a regex pattern (via ModelRgx/ProviderRgx),
// or a provider-only entry. Only models whose providers are configured and
// able to serve (have API key or auth_style "none") are returned.
func FindRegistryModels(pattern config.AgentModel, providers map[string]*config.Provider) []config.AgentModel {
	var models []config.AgentModel
	for modelID, entry := range config.ModelRegistry {
		if !pattern.MatchesCatalog(entry.Provider, modelID) {
			continue
		}
		if providers != nil {
			if p, ok := providers[entry.Provider]; !ok || !ProviderCanServe(p) {
				continue
			}
		}
		models = append(models, config.AgentModel{
			Provider:   entry.Provider,
			Model:      modelID,
			MaxContext: entry.MaxContext,
			MaxOutput:  entry.MaxOutput,
		})
	}
	return models
}
