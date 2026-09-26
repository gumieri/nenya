package discovery

import (
	"regexp"
	"testing"

	"github.com/nenya/config"
)

func TestBuildProviderNonChat(t *testing.T) {
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
			name: "no non-chat models",
			providers: map[string]config.ProviderConfig{
				"zen": {URL: "https://opencode.ai/zen/v1/chat/completions"},
			},
			wantNil: true,
		},
		{
			name: "one provider with non-chat models",
			providers: map[string]config.ProviderConfig{
				"zen": {NonChatModels: []string{`^jev-`}},
			},
			wantNil:   false,
			wantCount: 1,
		},
		{
			name: "invalid pattern is skipped",
			providers: map[string]config.ProviderConfig{
				"zen": {NonChatModels: []string{"("}},
			},
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildProviderNonChat(tt.providers)
			if (got == nil) != tt.wantNil {
				t.Errorf("buildProviderNonChat() = %v, wantNil %v", got, tt.wantNil)
			}
			if got != nil && len(got) != tt.wantCount {
				t.Errorf("buildProviderNonChat() length = %d, want %d", len(got), tt.wantCount)
			}
		})
	}
}

func TestIsNonChat(t *testing.T) {
	nonChat := map[string][]*regexp.Regexp{
		"zen": {regexp.MustCompile(`^jev-`)},
	}

	tests := []struct {
		name        string
		provider    string
		modelID     string
		wantNonChat bool
	}{
		{name: "nil map", provider: "zen", modelID: "jev-1.13", wantNonChat: false},
		{name: "provider not present", provider: "openai", modelID: "gpt-4", wantNonChat: false},
		{name: "match", provider: "zen", modelID: "jev-1.13-free", wantNonChat: true},
		{name: "no match", provider: "zen", modelID: "kimi-k3", wantNonChat: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := nonChat
			if tt.name == "nil map" {
				m = nil
			}
			if got := isNonChat(m, tt.provider, tt.modelID); got != tt.wantNonChat {
				t.Errorf("isNonChat() = %v, want %v", got, tt.wantNonChat)
			}
		})
	}
}

func TestMergeCatalog_NonChatModels(t *testing.T) {
	tests := []struct {
		name         string
		providers    map[string]config.ProviderConfig
		discovered   []DiscoveredModel
		wantExcluded []string
		wantIncluded []string
	}{
		{
			name: "non-chat model excluded, chat model kept",
			providers: map[string]config.ProviderConfig{
				"zen": {
					URL:           "https://opencode.ai/zen/v1/chat/completions",
					NonChatModels: []string{`^jev-`},
				},
			},
			discovered: []DiscoveredModel{
				{ID: "jev-1.13-free", Provider: "zen", MaxContext: 64000},
				{ID: "kimi-k3", Provider: "zen", MaxContext: 262144},
			},
			wantExcluded: []string{"zen/jev-1.13-free"},
			wantIncluded: []string{"zen/kimi-k3"},
		},
		{
			name: "no non-chat config keeps all",
			providers: map[string]config.ProviderConfig{
				"zen": {URL: "https://opencode.ai/zen/v1/chat/completions"},
			},
			discovered: []DiscoveredModel{
				{ID: "jev-1.13-free", Provider: "zen", MaxContext: 64000},
				{ID: "kimi-k3", Provider: "zen", MaxContext: 262144},
			},
			wantExcluded: []string{},
			wantIncluded: []string{"zen/jev-1.13-free", "zen/kimi-k3"},
		},
		{
			name: "non-chat pattern scoped to provider",
			providers: map[string]config.ProviderConfig{
				"zen":     {NonChatModels: []string{`^jev-`}},
				"otherai": {URL: "https://other.ai/v1"},
			},
			discovered: []DiscoveredModel{
				{ID: "jev-1.13-free", Provider: "zen", MaxContext: 64000},
				{ID: "jev-1.13-free", Provider: "otherai", MaxContext: 64000},
			},
			wantExcluded: []string{"zen/jev-1.13-free"},
			wantIncluded: []string{"otherai/jev-1.13-free"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			catalog := NewModelCatalog()
			for _, m := range tt.discovered {
				catalog.models[m.ID] = append(catalog.models[m.ID], m)
				catalog.providers[m.Provider] = append(catalog.providers[m.Provider], m.ID)
			}

			merged := MergeCatalog(catalog, &config.Config{Providers: tt.providers})

			excludedMap := make(map[string]bool)
			for _, id := range tt.wantExcluded {
				excludedMap[id] = true
			}
			includedMap := make(map[string]bool)
			for _, id := range tt.wantIncluded {
				includedMap[id] = true
			}

			for _, m := range merged.AllModels() {
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

func TestApplyProviderNonChatToCatalog(t *testing.T) {
	catalog := NewModelCatalog()
	for _, m := range []DiscoveredModel{
		{ID: "jev-1.13-free", Provider: "zen"},
		{ID: "kimi-k3", Provider: "zen"},
	} {
		catalog.models[m.ID] = append(catalog.models[m.ID], m)
		catalog.providers[m.Provider] = append(catalog.providers[m.Provider], m.ID)
	}

	ApplyProviderNonChatToCatalog(catalog, map[string]config.ProviderConfig{
		"zen": {NonChatModels: []string{`^jev-`}},
	})

	if _, ok := catalog.Lookup("jev-1.13-free"); ok {
		t.Fatal("ApplyProviderNonChatToCatalog() kept excluded model jev-1.13-free")
	}
	if _, ok := catalog.Lookup("kimi-k3"); !ok {
		t.Fatal("ApplyProviderNonChatToCatalog() removed chat model kimi-k3")
	}
	if got := catalog.ModelsForProvider("zen"); len(got) != 1 || got[0].ID != "kimi-k3" {
		t.Fatalf("ModelsForProvider() = %v, want only kimi-k3", got)
	}

	// nil catalog and empty providers must be no-ops.
	ApplyProviderNonChatToCatalog(nil, nil)
}
