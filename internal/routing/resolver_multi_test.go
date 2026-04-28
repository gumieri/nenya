package routing

import (
	"testing"

	"nenya/internal/config"
	"nenya/internal/discovery"
)

func TestResolveProviders_MultiProviderFromCatalog(t *testing.T) {
	catalog := discovery.NewModelCatalog()
	catalog.Add(discovery.DiscoveredModel{ID: "deepseek-v4-flash", Provider: "deepseek", MaxContext: 100000, MaxOutput: 393216})
	catalog.Add(discovery.DiscoveredModel{ID: "deepseek-v4-flash", Provider: "nvidia", MaxContext: 50000, MaxOutput: 16384})

	providers := map[string]*config.Provider{
		"deepseek": {Name: "deepseek", URL: "https://api.deepseek.com/chat/completions"},
		"nvidia":   {Name: "nvidia", URL: "https://integrate.api.nvidia.com/v1/chat/completions"},
	}

	matches := ResolveProviders("deepseek-v4-flash", providers, catalog)
	if len(matches) != 2 {
		t.Fatalf("expected 2 provider matches, got %d", len(matches))
	}

	providersFound := make(map[string]bool)
	for _, m := range matches {
		providersFound[m.Provider] = true
	}
	if !providersFound["deepseek"] || !providersFound["nvidia"] {
		t.Fatalf("expected providers deepseek and nvidia, got %v", providersFound)
	}

	for _, m := range matches {
		if m.Model != "deepseek-v4-flash" {
			t.Errorf("expected model deepseek-v4-flash, got %s", m.Model)
		}
	}
}

func TestResolveProviders_SingleProviderFromCatalog(t *testing.T) {
	catalog := discovery.NewModelCatalog()
	catalog.Add(discovery.DiscoveredModel{ID: "glm-5-turbo", Provider: "zai", MaxContext: 200000, MaxOutput: 128000})

	providers := map[string]*config.Provider{
		"zai": {Name: "zai", URL: "https://api.z.ai/api/paas/v4/chat/completions"},
	}

	matches := ResolveProviders("glm-5-turbo", providers, catalog)
	if len(matches) != 1 {
		t.Fatalf("expected 1 provider match, got %d", len(matches))
	}

	if matches[0].Provider != "zai" {
		t.Errorf("expected provider zai, got %s", matches[0].Provider)
	}
	if matches[0].Model != "glm-5-turbo" {
		t.Errorf("expected model glm-5-turbo, got %s", matches[0].Model)
	}
}

func TestResolveProviders_FallbackToModelRegistry(t *testing.T) {
	catalog := discovery.NewModelCatalog()
	providers := map[string]*config.Provider{
		"deepseek": {Name: "deepseek", URL: "https://api.deepseek.com/chat/completions"},
	}

	matches := ResolveProviders("deepseek-v4-pro", providers, catalog)
	if len(matches) != 1 {
		t.Fatalf("expected 1 provider match from ModelRegistry, got %d", len(matches))
	}

	if matches[0].Provider != "deepseek" {
		t.Errorf("expected provider deepseek, got %s", matches[0].Provider)
	}
	if matches[0].Model != "deepseek-v4-pro" {
		t.Errorf("expected model deepseek-v4-pro, got %s", matches[0].Model)
	}
}

func TestResolveProviders_NoMatch(t *testing.T) {
	catalog := discovery.NewModelCatalog()
	providers := map[string]*config.Provider{
		"deepseek": {Name: "deepseek", URL: "https://api.deepseek.com/chat/completions"},
	}

	matches := ResolveProviders("unknown-model", providers, catalog)
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches for unknown model, got %d", len(matches))
	}
}

func TestResolveProviders_CatalogTakesPriorityOverRegistry(t *testing.T) {
	catalog := discovery.NewModelCatalog()
	catalog.Add(discovery.DiscoveredModel{ID: "gpt-4o", Provider: "github", MaxContext: 8000, MaxOutput: 4096})

	providers := map[string]*config.Provider{
		"github": {Name: "github", URL: "https://models.inference.ai.azure.com/chat/completions"},
	}

	matches := ResolveProviders("gpt-4o", providers, catalog)
	if len(matches) != 1 {
		t.Fatalf("expected 1 provider match from catalog, got %d", len(matches))
	}

	if matches[0].Provider != "github" {
		t.Errorf("expected provider github from catalog, got %s", matches[0].Provider)
	}
}

func TestResolveProviders_MaxContextOutputFromCatalog(t *testing.T) {
	catalog := discovery.NewModelCatalog()
	catalog.Add(discovery.DiscoveredModel{ID: "test-model", Provider: "test", MaxContext: 1234, MaxOutput: 5678})

	providers := map[string]*config.Provider{
		"test": {Name: "test", URL: "https://api.example.com/chat/completions"},
	}

	matches := ResolveProviders("test-model", providers, catalog)
	if len(matches) != 1 {
		t.Fatalf("expected 1 provider match, got %d", len(matches))
	}

	if matches[0].MaxContext != 1234 {
		t.Errorf("expected MaxContext 1234, got %d", matches[0].MaxContext)
	}
	if matches[0].MaxOutput != 5678 {
		t.Errorf("expected MaxOutput 5678, got %d", matches[0].MaxOutput)
	}
}

func TestResolveProviders_NilCatalog(t *testing.T) {
	providers := targetProviders()

	matches := ResolveProviders("deepseek-v4-pro", providers, nil)
	if len(matches) != 1 {
		t.Fatalf("expected 1 provider match from ModelRegistry with nil catalog, got %d", len(matches))
	}

	if matches[0].Provider != "deepseek" {
		t.Errorf("expected provider deepseek from ModelRegistry, got %s", matches[0].Provider)
	}
}

func TestResolveProviders_NilCatalog_NoMatch(t *testing.T) {
	providers := map[string]*config.Provider{
		"deepseek": {Name: "deepseek", URL: "https://api.deepseek.com/chat/completions"},
	}

	matches := ResolveProviders("unknown-model", providers, nil)
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches with nil catalog and unknown model, got %d", len(matches))
	}
}
