package discovery

import (
	"regexp"
	"testing"

	"github.com/nenya/config"
)

func TestBuildProviderAllows(t *testing.T) {
	tests := []struct {
		name      string
		providers map[string]config.ProviderConfig
		wantNil   bool
		wantCount int
	}{
		{
			name:      "nil providers",
			providers: nil,
			wantNil:   true,
		},
		{
			name:      "empty providers",
			providers: map[string]config.ProviderConfig{},
			wantNil:   true,
		},
		{
			name: "no allowed models",
			providers: map[string]config.ProviderConfig{
				"openai": {URL: "https://api.openai.com"},
			},
			wantNil: true,
		},
		{
			name: "one provider with allowed models",
			providers: map[string]config.ProviderConfig{
				"openai": {
					AllowedModels: []string{"gpt-4", "gpt-3.5"},
				},
			},
			wantNil:   false,
			wantCount: 1,
		},
		{
			name: "multiple providers",
			providers: map[string]config.ProviderConfig{
				"openai": {
					AllowedModels: []string{"gpt-\\d+"},
				},
				"anthropic": {
					AllowedModels: []string{"claude-.*"},
				},
			},
			wantNil:   false,
			wantCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildProviderAllows(tt.providers)
			if (got == nil) != tt.wantNil {
				t.Errorf("buildProviderAllows() = %v, wantNil %v", got, tt.wantNil)
			}
			if got != nil && len(got) != tt.wantCount {
				t.Errorf("buildProviderAllows() length = %d, want %d", len(got), tt.wantCount)
			}
		})
	}
}

func TestIsModelAllowed(t *testing.T) {
	providerAllows := map[string][]*regexp.Regexp{
		"openai":    {regexp.MustCompile("gpt-\\d+"), regexp.MustCompile("gpt-4")},
		"anthropic": {regexp.MustCompile("claude-.*")},
	}

	tests := []struct {
		name           string
		providerAllows map[string][]*regexp.Regexp
		provider       string
		modelID        string
		wantAllowed    bool
	}{
		{
			name:           "nil allows allows all",
			providerAllows: nil,
			provider:       "openai",
			modelID:        "any-model",
			wantAllowed:    true,
		},
		{
			name:           "empty allows allows all",
			providerAllows: map[string][]*regexp.Regexp{},
			provider:       "openai",
			modelID:        "any-model",
			wantAllowed:    true,
		},
		{
			name:           "provider not in allows",
			providerAllows: providerAllows,
			provider:       "google",
			modelID:        "gemini-pro",
			wantAllowed:    true,
		},
		{
			name:           "exact match in allowed list",
			providerAllows: providerAllows,
			provider:       "openai",
			modelID:        "gpt-4",
			wantAllowed:    true,
		},
		{
			name:           "regex match in allowed list",
			providerAllows: providerAllows,
			provider:       "openai",
			modelID:        "gpt-3.5",
			wantAllowed:    true,
		},
		{
			name:           "no match in allowed list",
			providerAllows: providerAllows,
			provider:       "openai",
			modelID:        "claude-3",
			wantAllowed:    false,
		},
		{
			name:           "different provider regex match",
			providerAllows: providerAllows,
			provider:       "anthropic",
			modelID:        "claude-3-opus",
			wantAllowed:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isModelAllowed(tt.providerAllows, tt.provider, tt.modelID); got != tt.wantAllowed {
				t.Errorf("isModelAllowed() = %v, want %v", got, tt.wantAllowed)
			}
		})
	}
}

func TestMergeCatalog_ProviderAllows(t *testing.T) {
	tests := []struct {
		name         string
		providers    map[string]config.ProviderConfig
		discovered   []DiscoveredModel
		wantExcluded []string
		wantIncluded []string
	}{
		{
			name: "no provider allows",
			providers: map[string]config.ProviderConfig{
				"openai": {URL: "https://api.openai.com"},
			},
			discovered: []DiscoveredModel{
				{ID: "gpt-4", Provider: "openai", MaxContext: 128000},
			},
			wantExcluded: []string{},
			wantIncluded: []string{"openai/gpt-4"},
		},
		{
			name: "provider allows exact match",
			providers: map[string]config.ProviderConfig{
				"openai": {
					URL:           "https://api.openai.com",
					AllowedModels: []string{"gpt-4"},
				},
			},
			discovered: []DiscoveredModel{
				{ID: "gpt-4", Provider: "openai", MaxContext: 128000},
				{ID: "gpt-3.5", Provider: "openai", MaxContext: 16000},
			},
			wantExcluded: []string{"openai/gpt-3.5"},
			wantIncluded: []string{"openai/gpt-4"},
		},
		{
			name: "provider allows regex match",
			providers: map[string]config.ProviderConfig{
				"openai": {
					URL:           "https://api.openai.com",
					AllowedModels: []string{"gpt-\\d+"},
				},
				"anthropic": {
					URL:           "https://api.anthropic.com",
					AllowedModels: []string{"claude-.*"},
				},
			},
			discovered: []DiscoveredModel{
				{ID: "gpt-4", Provider: "openai", MaxContext: 128000},
				{ID: "claude-3", Provider: "anthropic", MaxContext: 200000},
			},
			wantExcluded: []string{},
			wantIncluded: []string{"openai/gpt-4", "anthropic/claude-3"},
		},
		{
			name: "static registry backfill with provider allows",
			providers: map[string]config.ProviderConfig{
				"openai": {
					URL:           "https://api.openai.com",
					AllowedModels: []string{"gpt-4"},
				},
			},
			discovered: []DiscoveredModel{
				{ID: "gpt-4", Provider: "openai", MaxContext: 128000},
			},
			wantExcluded: []string{},
			wantIncluded: []string{"openai/gpt-4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			catalog := NewModelCatalog()
			for _, m := range tt.discovered {
				catalog.models[m.ID] = append(catalog.models[m.ID], m)
				catalog.providers[m.Provider] = append(catalog.providers[m.Provider], m.ID)
			}

			cfg := &config.Config{
				Providers: tt.providers,
			}

			merged := MergeCatalog(catalog, cfg)
			mergedModels := merged.AllModels()

			excludedMap := make(map[string]bool)
			for _, id := range tt.wantExcluded {
				excludedMap[id] = true
			}
			includedMap := make(map[string]bool)
			for _, id := range tt.wantIncluded {
				includedMap[id] = true
			}

			for _, m := range mergedModels {
				fullID := m.Provider + "/" + m.ID
				if excludedMap[fullID] {
					t.Errorf("MergeCatalog() included excluded model %s", fullID)
				}
				delete(includedMap, fullID)
			}

			for id := range includedMap {
				t.Errorf("MergeCatalog() excluded expected model %s", id)
			}
		})
	}
}

func TestMergeCatalog_StaticRegistryWithProviderAllows(t *testing.T) {
	tests := []struct {
		name         string
		providers    map[string]config.ProviderConfig
		discovered   []DiscoveredModel
		wantIncluded []string
		wantExcluded []string
	}{
		{
			name: "static model excluded by provider",
			providers: map[string]config.ProviderConfig{
				"openai": {
					URL:           "https://api.openai.com",
					AllowedModels: []string{"^gpt-\\d+$"},
				},
			},
			discovered: []DiscoveredModel{
				{ID: "gpt-4-turbo", Provider: "openai", MaxContext: 128000},
			},
			wantExcluded: []string{"openai/gpt-4-turbo"},
			wantIncluded: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			catalog := NewModelCatalog()
			for _, m := range tt.discovered {
				catalog.models[m.ID] = append(catalog.models[m.ID], m)
				catalog.providers[m.Provider] = append(catalog.providers[m.Provider], m.ID)
			}

			cfg := &config.Config{
				Providers: tt.providers,
			}

			merged := MergeCatalog(catalog, cfg)
			mergedModels := merged.AllModels()

			excludedMap := make(map[string]bool)
			for _, id := range tt.wantExcluded {
				excludedMap[id] = true
			}
			includedMap := make(map[string]bool)
			for _, id := range tt.wantIncluded {
				includedMap[id] = true
			}

			for _, m := range mergedModels {
				fullID := m.Provider + "/" + m.ID
				if excludedMap[fullID] {
					t.Errorf("MergeCatalog() included excluded model %s", fullID)
				}
				delete(includedMap, fullID)
			}

			for id := range includedMap {
				t.Errorf("MergeCatalog() excluded expected model %s", id)
			}
		})
	}
}
