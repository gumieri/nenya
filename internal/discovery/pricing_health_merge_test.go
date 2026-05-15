package discovery

import (
	"context"
	"math"
	"testing"
	"time"

	"nenya/config"
	"nenya/internal/testutil"
)

func TestPricingEntry_IsZero(t *testing.T) {
	tests := []struct {
		name string
		p    PricingEntry
		zero bool
	}{
		{"all zero", PricingEntry{}, true},
		{"input cost set", PricingEntry{InputCostPer1M: 1.0}, false},
		{"output cost set", PricingEntry{OutputCostPer1M: 1.0}, false},
		{"both set", PricingEntry{InputCostPer1M: 1.0, OutputCostPer1M: 0.5}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.IsZero(); got != tt.zero {
				t.Errorf("IsZero() = %v, want %v", got, tt.zero)
			}
		})
	}
}

func TestPricingEntry_CalculateCost(t *testing.T) {
	tests := []struct {
		name         string
		p            PricingEntry
		inputTokens  int64
		outputTokens int64
		want         float64
	}{
		{"zero tokens", PricingEntry{InputCostPer1M: 1.0, OutputCostPer1M: 2.0}, 0, 0, 0},
		{"only input", PricingEntry{InputCostPer1M: 1.0, OutputCostPer1M: 0}, 1_000_000, 0, 1.0},
		{"only output", PricingEntry{InputCostPer1M: 0, OutputCostPer1M: 2.0}, 0, 500_000, 1.0},
		{"both", PricingEntry{InputCostPer1M: 1.0, OutputCostPer1M: 2.0}, 1_000_000, 500_000, 2.0},
		{"fractional tokens", PricingEntry{InputCostPer1M: 3.0}, 500_000, 0, 1.5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.p.CalculateCost(tt.inputTokens, tt.outputTokens)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("CalculateCost() = %f, want %f", got, tt.want)
			}
		})
	}
}

func TestNewStaticPricing(t *testing.T) {
	sp := NewStaticPricing(map[string]PricingEntry{
		"model-a": {InputCostPer1M: 1.0, OutputCostPer1M: 2.0},
	})
	if sp == nil {
		t.Fatal("expected non-nil StaticPricing")
	}
}

func TestStaticPricing_GetPricing(t *testing.T) {
	ctx := context.Background()

	t.Run("found", func(t *testing.T) {
		sp := NewStaticPricing(map[string]PricingEntry{
			"model-a": {InputCostPer1M: 1.0},
		})
		p, ok := sp.GetPricing(ctx, "model-a")
		if !ok {
			t.Fatal("expected ok=true")
		}
		if p.InputCostPer1M != 1.0 {
			t.Errorf("expected input cost 1.0, got %f", p.InputCostPer1M)
		}
	})

	t.Run("not found", func(t *testing.T) {
		sp := NewStaticPricing(map[string]PricingEntry{
			"model-a": {InputCostPer1M: 1.0},
		})
		_, ok := sp.GetPricing(ctx, "model-b")
		if ok {
			t.Fatal("expected ok=false")
		}
	})

	t.Run("nil map", func(t *testing.T) {
		sp := NewStaticPricing(nil)
		_, ok := sp.GetPricing(ctx, "any")
		if ok {
			t.Fatal("expected ok=false for nil map")
		}
	})
}

func TestNewFallbackPricing(t *testing.T) {
	fp := NewFallbackPricing(2.0, 5.0)
	if fp == nil {
		t.Fatal("expected non-nil FallbackPricing")
	}
}

func TestFallbackPricing_GetPricing(t *testing.T) {
	ctx := context.Background()
	fp := NewFallbackPricing(2.0, 5.0)
	p, ok := fp.GetPricing(ctx, "any-model")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if p.InputCostPer1M != 2.0 {
		t.Errorf("expected input cost 2.0, got %f", p.InputCostPer1M)
	}
	if p.OutputCostPer1M != 5.0 {
		t.Errorf("expected output cost 5.0, got %f", p.OutputCostPer1M)
	}
	if p.Currency != "USD" {
		t.Errorf("expected currency USD, got %s", p.Currency)
	}
}

func TestMergePricing(t *testing.T) {
	t.Run("merge without overlap", func(t *testing.T) {
		disc := map[string]PricingEntry{
			"model-a": {InputCostPer1M: 1.0},
		}
		static := map[string]PricingEntry{
			"model-b": {InputCostPer1M: 2.0},
		}
		merged := MergePricing(disc, static)
		if len(merged) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(merged))
		}
		if merged["model-a"].InputCostPer1M != 1.0 {
			t.Error("expected model-a pricing from discovered")
		}
		if merged["model-b"].InputCostPer1M != 2.0 {
			t.Error("expected model-b pricing from static")
		}
	})

	t.Run("discovered takes priority", func(t *testing.T) {
		disc := map[string]PricingEntry{
			"model-a": {InputCostPer1M: 1.0},
		}
		static := map[string]PricingEntry{
			"model-a": {InputCostPer1M: 2.0},
		}
		merged := MergePricing(disc, static)
		if len(merged) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(merged))
		}
		if merged["model-a"].InputCostPer1M != 1.0 {
			t.Errorf("expected discovered pricing 1.0, got %f", merged["model-a"].InputCostPer1M)
		}
	})

	t.Run("static fills zero discovered", func(t *testing.T) {
		disc := map[string]PricingEntry{
			"model-a": {},
		}
		static := map[string]PricingEntry{
			"model-a": {InputCostPer1M: 2.0},
		}
		merged := MergePricing(disc, static)
		if len(merged) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(merged))
		}
		if merged["model-a"].InputCostPer1M != 2.0 {
			t.Errorf("expected static pricing 2.0, got %f", merged["model-a"].InputCostPer1M)
		}
	})
}

func TestNewHealthRegistry(t *testing.T) {
	r := NewHealthRegistry()
	if r == nil {
		t.Fatal("expected non-nil HealthRegistry")
	}
}

func TestHealthRegistry_UpdateAndGet(t *testing.T) {
	r := NewHealthRegistry()
	now := time.Now()

	r.Update("openai", ProviderHealth{
		Name:        "openai",
		Status:      "ok",
		ModelsFound: 10,
		LastFetched: now,
	})

	health, ok := r.Get("openai")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if health.Name != "openai" || health.Status != "ok" || health.ModelsFound != 10 {
		t.Errorf("unexpected health: %+v", health)
	}

	_, ok = r.Get("nonexistent")
	if ok {
		t.Fatal("expected ok=false for nonexistent")
	}
}

func TestHealthRegistry_Snapshot(t *testing.T) {
	r := NewHealthRegistry()
	r.Update("p1", ProviderHealth{Name: "p1", Status: "ok"})
	r.Update("p2", ProviderHealth{Name: "p2", Status: "unreachable"})

	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(snap))
	}
	if snap["p1"].Status != "ok" {
		t.Errorf("expected p1 ok, got %s", snap["p1"].Status)
	}
}

func TestValidateProviderHealth(t *testing.T) {
	logger := testutil.NewTestLogger()

	t.Run("no api key", func(t *testing.T) {
		provider := &config.Provider{
			Name:      "test",
			AuthStyle: "bearer",
		}
		catalog := NewModelCatalog()
		health := ValidateProviderHealth("test", provider, catalog, logger)
		if health.Status != HealthStatusUnreachable {
			t.Errorf("expected unreachable, got %s", health.Status)
		}
	})

	t.Run("no auth style not unreachable", func(t *testing.T) {
		provider := &config.Provider{
			Name:      "test",
			AuthStyle: "none",
		}
		catalog := NewModelCatalog()
		health := ValidateProviderHealth("test", provider, catalog, logger)
		if health.Status != HealthStatusEmpty {
			t.Errorf("expected empty, got %s", health.Status)
		}
	})

	t.Run("has models", func(t *testing.T) {
		provider := &config.Provider{
			Name:      "test",
			AuthStyle: "none",
		}
		catalog := NewModelCatalog()
		catalog.Add(DiscoveredModel{ID: "model-a", Provider: "test"})
		health := ValidateProviderHealth("test", provider, catalog, logger)
		if health.Status != HealthStatusOK {
			t.Errorf("expected ok, got %s", health.Status)
		}
		if health.ModelsFound != 1 {
			t.Errorf("expected 1 model, got %d", health.ModelsFound)
		}
	})
}

func TestValidateAllProviders(t *testing.T) {
	logger := testutil.NewTestLogger()

	providers := map[string]*config.Provider{
		"with-key": {
			Name:      "with-key",
			AuthStyle: "bearer",
			APIKey:    "sk-test",
		},
		"no-key-auth": {
			Name:      "no-key-auth",
			AuthStyle: "none",
		},
	}

	catalog := NewModelCatalog()
	catalog.Add(DiscoveredModel{ID: "model-a", Provider: "with-key"})
	catalog.Add(DiscoveredModel{ID: "model-b", Provider: "no-key-auth"})

	registry := ValidateAllProviders(providers, catalog, logger)
	snap := registry.Snapshot()

	if _, ok := snap["with-key"]; !ok {
		t.Error("expected with-key in registry")
	}
	if _, ok := snap["no-key-auth"]; !ok {
		t.Error("expected no-key-auth in registry")
	}
}

func TestValidateAllProviders_SkipsNoKey(t *testing.T) {
	logger := testutil.NewTestLogger()
	providers := map[string]*config.Provider{
		"no-key": {
			Name:      "no-key",
			AuthStyle: "bearer",
		},
	}

	catalog := NewModelCatalog()
	catalog.Add(DiscoveredModel{ID: "model-a", Provider: "no-key"})

	registry := ValidateAllProviders(providers, catalog, logger)
	snap := registry.Snapshot()
	if _, ok := snap["no-key"]; ok {
		t.Error("expected no-key to be skipped")
	}
}

func TestMergeCatalog(t *testing.T) {
	t.Run("empty catalog and registry", func(t *testing.T) {
		catalog := NewModelCatalog()
		cfg := &config.Config{
			Providers: map[string]config.ProviderConfig{},
			Agents:    map[string]config.AgentConfig{},
		}
		merged := MergeCatalog(catalog, cfg)
		if merged == nil {
			t.Fatal("expected non-nil merged catalog")
		}
		models := merged.AllModels()
		if len(models) != len(config.ModelRegistry) {
			t.Errorf("expected %d models from registry, got %d", len(config.ModelRegistry), len(models))
		}
	})

	t.Run("with discovered models", func(t *testing.T) {
		catalog := NewModelCatalog()
		catalog.Add(DiscoveredModel{
			ID:         "deepseek-v4-flash",
			Provider:   "custom-provider",
			MaxContext: 999999,
			MaxOutput:  999999,
		})
		cfg := &config.Config{
			Providers: map[string]config.ProviderConfig{},
			Agents:    map[string]config.AgentConfig{},
		}
		merged := MergeCatalog(catalog, cfg)

		models := merged.ModelsForProvider("custom-provider")
		if len(models) == 0 {
			t.Fatal("expected custom-provider models")
		}
		if models[0].MaxContext != 999999 {
			t.Errorf("expected discovered MaxContext to override")
		}
	})

	t.Run("with agent overrides", func(t *testing.T) {
		catalog := NewModelCatalog()
		cfg := &config.Config{
			Agents: map[string]config.AgentConfig{
				"test-agent": {
					Models: []config.AgentModel{
						{Model: "deepseek-v4-flash", Provider: "override-provider", MaxContext: 50000},
					},
				},
			},
			Providers: map[string]config.ProviderConfig{},
		}
		merged := MergeCatalog(catalog, cfg)

		models := merged.ModelsForProvider("override-provider")
		if len(models) == 0 {
			t.Fatal("expected override-provider models")
		}
		if models[0].MaxContext != 50000 {
			t.Errorf("expected MaxContext 50000, got %d", models[0].MaxContext)
		}
	})
}

func TestBuildAgentOverrides(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		overrides := buildAgentOverrides(nil)
		if len(overrides) != 0 {
			t.Errorf("expected empty overrides, got %d", len(overrides))
		}
	})

	t.Run("no agents", func(t *testing.T) {
		cfg := &config.Config{}
		overrides := buildAgentOverrides(cfg)
		if len(overrides) != 0 {
			t.Errorf("expected empty overrides, got %d", len(overrides))
		}
	})

	t.Run("with model overrides", func(t *testing.T) {
		cfg := &config.Config{
			Agents: map[string]config.AgentConfig{
				"agent-a": {
					Models: []config.AgentModel{
						{Model: "model-x", Provider: "p1", MaxContext: 100000, MaxOutput: 50000},
					},
				},
			},
		}
		overrides := buildAgentOverrides(cfg)
		if len(overrides) != 1 {
			t.Fatalf("expected 1 override, got %d", len(overrides))
		}
		o := overrides["model-x"]
		if o.Provider != "p1" || o.MaxContext != 100000 || o.MaxOutput != 50000 {
			t.Errorf("unexpected override: %+v", o)
		}
	})

	t.Run("multiple agents same model merges", func(t *testing.T) {
		cfg := &config.Config{
			Agents: map[string]config.AgentConfig{
				"agent-a": {Models: []config.AgentModel{{Model: "model-x", Provider: "p1"}}},
				"agent-b": {Models: []config.AgentModel{{Model: "model-x", MaxContext: 99999}}},
			},
		}
		overrides := buildAgentOverrides(cfg)
		o := overrides["model-x"]
		if o.Provider != "p1" || o.MaxContext != 99999 {
			t.Errorf("unexpected merged override: %+v", o)
		}
	})
}

func TestFirstNonEmpty(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{"first non-empty", []string{"", "hello", "world"}, "hello"},
		{"all empty", []string{"", ""}, ""},
		{"single value", []string{"only"}, "only"},
		{"empty list", []string{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstNonEmpty(tt.values...); got != tt.want {
				t.Errorf("firstNonEmpty() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFirstPositive(t *testing.T) {
	tests := []struct {
		name   string
		values []int
		want   int
	}{
		{"first positive", []int{0, 5, 10}, 5},
		{"all zero", []int{0, 0}, 0},
		{"negative values", []int{-1, 0, 3}, 3},
		{"empty list", []int{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstPositive(tt.values...); got != tt.want {
				t.Errorf("firstPositive() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestPickProvider(t *testing.T) {
	tests := []struct {
		name      string
		staticOk  bool
		staticVal string
		discOk    bool
		discVal   string
		want      string
	}{
		{"static exists", true, "p1", false, "", "p1"},
		{"static missing, discovered exists", false, "", true, "p2", "p2"},
		{"both missing", false, "", false, "", ""},
		{"static empty, discovered exists", true, "", true, "p3", "p3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pickProvider(tt.staticOk, tt.staticVal, tt.discOk, tt.discVal); got != tt.want {
				t.Errorf("pickProvider() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPickFormat(t *testing.T) {
	tests := []struct {
		name      string
		staticOk  bool
		staticVal string
		discOk    bool
		discVal   string
		want      string
	}{
		{"static exists", true, "openai", false, "", "openai"},
		{"discovered only", false, "", true, "anthropic", "anthropic"},
		{"static empty, discovered exists", true, "", true, "gemini", "gemini"},
		{"neither", false, "", false, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pickFormat(tt.staticOk, tt.staticVal, tt.discOk, tt.discVal); got != tt.want {
				t.Errorf("pickFormat() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPickInt(t *testing.T) {
	t.Run("exists", func(t *testing.T) {
		if got := pickInt(true, 42); got != 42 {
			t.Errorf("expected 42, got %d", got)
		}
	})
	t.Run("not exists", func(t *testing.T) {
		if got := pickInt(false, 42); got != 0 {
			t.Errorf("expected 0, got %d", got)
		}
	})
}
