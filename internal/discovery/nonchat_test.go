package discovery

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
)

// renderMetrics renders a metrics registry in the Prometheus text format so a
// test can assert a counter value.
func renderMetrics(t *testing.T, metrics *infra.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	infra.HandleMetrics(metrics, rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

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

// TestMergeCatalog_ProviderAgnosticDefault pins the NENYA-180 gap closure at the
// catalog level: once config.ApplyDefaults has run (the production load path),
// the provider-agnostic non_chat_models defaults reach EVERY provider, so a
// decision-only model discovered on a provider that declares nothing itself is
// still excluded from the chat catalog.
func TestMergeCatalog_ProviderAgnosticDefault(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"mygateway": {URL: "https://gw.example/v1/chat/completions", AuthStyle: "bearer"},
	}}
	if err := config.ApplyDefaults(cfg); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}

	catalog := NewModelCatalog()
	catalog.Add(DiscoveredModel{ID: "jev-1.13-free", Provider: "mygateway", MaxContext: 64000})
	catalog.Add(DiscoveredModel{ID: "chat-model-1", Provider: "mygateway", MaxContext: 128000})

	merged, drops := MergeCatalogWithDrops(catalog, cfg)

	if len(merged.LookupAll("jev-1.13-free")) != 0 {
		t.Errorf("a decision-only model discovered on a provider with no explicit config must not be chat-routable")
	}
	if len(merged.LookupAll("chat-model-1")) == 0 {
		t.Errorf("an ordinary model must survive the provider-agnostic default")
	}
	if got := drops["mygateway"]; len(got) != 1 || got[0] != "jev-1.13-free" {
		t.Errorf("drops = %v, want mygateway -> [jev-1.13-free]", drops)
	}
}

// TestMergeCatalogWithDrops_NoDropsIsEmpty pins that the report is empty (not a
// spurious entry) when nothing is classified non-chat, so the gateway's counter
// cannot drift.
func TestMergeCatalogWithDrops_NoDropsIsEmpty(t *testing.T) {
	catalog := NewModelCatalog()
	catalog.Add(DiscoveredModel{ID: "chat-model-1", Provider: "mygateway"})

	_, drops := MergeCatalogWithDrops(catalog, &config.Config{Providers: map[string]config.ProviderConfig{
		"mygateway": {URL: "https://gw.example/v1/chat/completions"},
	}})
	if len(drops) != 0 {
		t.Fatalf("drops = %v, want none", drops)
	}
}

// TestApplyProviderNonChatToCatalog_ReportsDrops pins the report from the
// pre-catalog path (discovery disabled, and the pricing-fetcher catalog).
func TestApplyProviderNonChatToCatalog_ReportsDrops(t *testing.T) {
	catalog := NewModelCatalog()
	catalog.Add(DiscoveredModel{ID: "jev-1.13-free", Provider: "zen"})
	catalog.Add(DiscoveredModel{ID: "jev-2", Provider: "zen"})
	catalog.Add(DiscoveredModel{ID: "kimi-k3", Provider: "zen"})

	drops := ApplyProviderNonChatToCatalog(catalog, map[string]config.ProviderConfig{
		"zen": {NonChatModels: []string{`^jev-`}},
	})
	if len(drops["zen"]) != 2 {
		t.Fatalf("drops[zen] = %v, want the two jev models", drops["zen"])
	}
	if len(catalog.LookupAll("kimi-k3")) == 0 {
		t.Error("the chat model must survive")
	}
	if len(catalog.LookupAll("jev-2")) != 0 {
		t.Error("the decision-only model must be removed")
	}

	// No classification configured: no report, nothing removed.
	clean := NewModelCatalog()
	clean.Add(DiscoveredModel{ID: "kimi-k3", Provider: "zen"})
	if drops := ApplyProviderNonChatToCatalog(clean, map[string]config.ProviderConfig{"zen": {URL: "x"}}); drops != nil {
		t.Fatalf("drops = %v, want nil when nothing is classified", drops)
	}
}

// TestFilterNonChatModels_CountsDrops pins the fetch-time filter: the drop is
// removed, logged, and counted under the provider label so the classification is
// observable in nenya_non_chat_models_dropped_total.
func TestFilterNonChatModels_CountsDrops(t *testing.T) {
	providers := config.ResolveProviders(
		&config.Config{Providers: map[string]config.ProviderConfig{
			"mygateway": {URL: "https://gw.example/v1/chat/completions", AuthStyle: "bearer", NonChatModels: []string{`^jev-`}},
		}},
		&config.SecretsConfig{ProviderKeys: map[string]string{"mygateway": "k"}},
	)
	provider := providers["mygateway"]
	if provider == nil {
		t.Fatal("mygateway did not resolve")
	}
	metrics := infra.NewMetrics()
	models := []DiscoveredModel{
		{ID: "jev-1.13-free", Provider: "mygateway"},
		{ID: "jev-2", Provider: "mygateway"},
		{ID: "chat-model-1", Provider: "mygateway"},
	}
	kept := filterNonChatModels(models, provider, slog.New(slog.DiscardHandler), metrics)
	if len(kept) != 1 || kept[0].ID != "chat-model-1" {
		t.Fatalf("kept = %v, want only chat-model-1", kept)
	}
	rendered := renderMetrics(t, metrics)
	if !strings.Contains(rendered, `nenya_non_chat_models_dropped_total{provider="mygateway"} 2`) {
		t.Fatalf("the drop counter is missing or wrong:\n%s", rendered)
	}
}
