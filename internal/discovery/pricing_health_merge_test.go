package discovery

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/testutil"
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
		{"peak-only is not zero", PricingEntry{PeakInputCostPer1M: 0.3, PeakOutputCostPer1M: 1.2}, false},
		{"cached-only is not zero", PricingEntry{CachedInputCostPer1M: 0.05}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.IsZero(); got != tt.zero {
				t.Errorf("IsZero() = %v, want %v", got, tt.zero)
			}
		})
	}
}

func TestPricingEntry_HasPeak(t *testing.T) {
	tests := []struct {
		name string
		p    PricingEntry
		want bool
	}{
		{"flat", PricingEntry{InputCostPer1M: 1}, false},
		{"peak input", PricingEntry{PeakInputCostPer1M: 1}, true},
		{"peak output", PricingEntry{PeakOutputCostPer1M: 1}, true},
		{"cached only", PricingEntry{CachedInputCostPer1M: 1}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.HasPeak(); got != tt.want {
				t.Errorf("HasPeak() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPricingEntry_CalculateCost(t *testing.T) {
	flat := PricingEntry{InputCostPer1M: 1.0, OutputCostPer1M: 2.0}
	peaked := PricingEntry{
		InputCostPer1M:       0.15,
		OutputCostPer1M:      0.60,
		PeakInputCostPer1M:   0.30,
		PeakOutputCostPer1M:  1.20,
		CachedInputCostPer1M: 0.05,
	}
	tests := []struct {
		name string
		p    PricingEntry
		u    PricingUsage
		want float64
	}{
		{"zero tokens", flat, PricingUsage{}, 0},
		{"only input", flat, PricingUsage{Input: 1_000_000}, 1.0},
		{"only output", flat, PricingUsage{Output: 500_000}, 1.0},
		{"both", flat, PricingUsage{Input: 1_000_000, Output: 500_000}, 2.0},
		{"fractional tokens", flat, PricingUsage{Input: 500_000}, 0.5},
		{"peak window", peaked, PricingUsage{Input: 1_000_000, Peak: true}, 0.30},
		{"peak off (standard pair)", peaked, PricingUsage{Input: 1_000_000}, 0.15},
		{"peak window no peak configured falls back to standard", flat, PricingUsage{Input: 1_000_000, Peak: true}, 1.0},
		{"partial peak pair falls back per dimension", PricingEntry{InputCostPer1M: 0.15, OutputCostPer1M: 0.60, PeakInputCostPer1M: 0.30}, PricingUsage{Input: 1_000_000, Output: 1_000_000, Peak: true}, 0.90},
		{"cached input uses cached rate", peaked, PricingUsage{Input: 1_000_000, CachedInput: 1_000_000}, 0.05},
		{"cached subset", peaked, PricingUsage{Input: 1_000_000, CachedInput: 400_000}, 0.4*0.05 + 0.6*0.15},
		{"cached clamps to input", peaked, PricingUsage{Input: 100_000, CachedInput: 500_000}, 0.005},
		{"negative cached clamps to zero", flat, PricingUsage{Input: 1_000_000, CachedInput: -100}, 1.0},
		{"negative clamps", flat, PricingUsage{Input: -5, Output: -5}, 0},
		{"cached without cached rate uses window input", flat, PricingUsage{Input: 1_000_000, CachedInput: 1_000_000}, 1.0},
		{"peak with cached", peaked, PricingUsage{Input: 1_000_000, CachedInput: 1_000_000, Peak: true}, 0.05},
		{"cached fallback under peak uses peak input", PricingEntry{InputCostPer1M: 0.15, OutputCostPer1M: 0.60, PeakInputCostPer1M: 0.30, PeakOutputCostPer1M: 1.20}, PricingUsage{Input: 1_000_000, CachedInput: 1_000_000, Peak: true}, 0.30},
		{"degenerate zero input rate with cached", PricingEntry{OutputCostPer1M: 2.0}, PricingUsage{Input: 1_000_000, CachedInput: 1_000_000}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.p.CalculateCost(tt.u)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("CalculateCost(%+v) = %f, want %f", tt.u, got, tt.want)
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

	t.Run("discovered peak-only is not zero and shadows static flat", func(t *testing.T) {
		disc := map[string]PricingEntry{
			"model-a": {PeakInputCostPer1M: 0.3, PeakOutputCostPer1M: 1.2},
		}
		static := map[string]PricingEntry{
			"model-a": {InputCostPer1M: 2.0, OutputCostPer1M: 4.0},
		}
		merged := MergePricing(disc, static)
		got := merged["model-a"]
		if got.IsZero() {
			t.Error("peak-only discovered entry must not be zero")
		}
		if got.HasStandardRate() {
			t.Error("peak-only discovered entry must not report a standard rate (static flat was shadowed)")
		}
		if got.PeakInputCostPer1M != 0.3 {
			t.Errorf("peak input = %f, want 0.3", got.PeakInputCostPer1M)
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

func TestMergeCatalog_PreservesAllMultiProviderEntries(t *testing.T) {
	t.Run("discovered-only model from multiple providers", func(t *testing.T) {
		catalog := NewModelCatalog()
		catalog.Add(DiscoveredModel{ID: "glm-5.1", Provider: "zen", MaxContext: 100000, MaxOutput: 8192})
		catalog.Add(DiscoveredModel{ID: "glm-5.1", Provider: "nvidia", MaxContext: 50000, MaxOutput: 4096})
		catalog.Add(DiscoveredModel{ID: "glm-5.1", Provider: "zai", MaxContext: 200000, MaxOutput: 16384})
		catalog.Add(DiscoveredModel{ID: "glm-5.1", Provider: "other", MaxContext: 75000, MaxOutput: 6000})

		cfg := &config.Config{
			Providers: map[string]config.ProviderConfig{},
			Agents:    map[string]config.AgentConfig{},
		}
		merged := MergeCatalog(catalog, cfg)

		entries := merged.LookupAll("glm-5.1")
		if len(entries) != 4 {
			t.Fatalf("expected 4 multi-provider entries, got %d", len(entries))
		}

		providers := make(map[string]bool)
		for _, e := range entries {
			providers[e.Provider] = true
		}
		expectedProviders := map[string]bool{"zen": true, "nvidia": true, "zai": true, "other": true}
		for p := range expectedProviders {
			if !providers[p] {
				t.Errorf("missing provider %s in merged catalog", p)
			}
		}
	})

	t.Run("model in static registry with multiple discovered providers", func(t *testing.T) {
		catalog := NewModelCatalog()
		catalog.Add(DiscoveredModel{ID: "glm-5.1", Provider: "zen", MaxContext: 100000, MaxOutput: 8192})
		catalog.Add(DiscoveredModel{ID: "glm-5.1", Provider: "nvidia", MaxContext: 50000, MaxOutput: 4096})
		catalog.Add(DiscoveredModel{ID: "glm-5.1", Provider: "zai", MaxContext: 200000, MaxOutput: 16384})

		cfg := &config.Config{
			Providers: map[string]config.ProviderConfig{},
			Agents:    map[string]config.AgentConfig{},
		}

		merged := MergeCatalog(catalog, cfg)

		entries := merged.LookupAll("glm-5.1")
		if len(entries) != 3 {
			t.Fatalf("expected 3 multi-provider entries, got %d", len(entries))
		}

		providers := make(map[string]bool)
		for _, e := range entries {
			providers[e.Provider] = true
		}
		expectedProviders := map[string]bool{"zen": true, "nvidia": true, "zai": true}
		for p := range expectedProviders {
			if !providers[p] {
				t.Errorf("missing provider %s in merged catalog", p)
			}
		}
	})

	t.Run("model with override preserves all discovered providers", func(t *testing.T) {
		catalog := NewModelCatalog()
		catalog.Add(DiscoveredModel{ID: "glm-5.1", Provider: "zen", MaxContext: 100000, MaxOutput: 8192})
		catalog.Add(DiscoveredModel{ID: "glm-5.1", Provider: "nvidia", MaxContext: 50000, MaxOutput: 4096})
		catalog.Add(DiscoveredModel{ID: "glm-5.1", Provider: "zai", MaxContext: 200000, MaxOutput: 16384})

		cfg := &config.Config{
			Agents: map[string]config.AgentConfig{
				"test-agent": {
					Models: []config.AgentModel{
						{Model: "glm-5.1", Provider: "override", MaxContext: 30000},
					},
				},
			},
			Providers: map[string]config.ProviderConfig{},
		}

		merged := MergeCatalog(catalog, cfg)

		entries := merged.LookupAll("glm-5.1")
		if len(entries) != 4 {
			t.Fatalf("expected 4 entries (1 override + 3 discovered), got %d", len(entries))
		}

		providers := make(map[string]bool)
		for _, e := range entries {
			providers[e.Provider] = true
		}
		expectedProviders := map[string]bool{"override": true, "zen": true, "nvidia": true, "zai": true}
		for p := range expectedProviders {
			if !providers[p] {
				t.Errorf("missing provider %s in merged catalog", p)
			}
		}
	})
}

func TestMergeCatalog_DeduplicationEdgeCases(t *testing.T) {
	t.Run("duplicate provider in raw catalog", func(t *testing.T) {
		catalog := NewModelCatalog()
		catalog.Add(DiscoveredModel{ID: "nonexistent-model", Provider: "zen", MaxContext: 100000, MaxOutput: 8192})
		catalog.Add(DiscoveredModel{ID: "nonexistent-model", Provider: "zen", MaxContext: 50000, MaxOutput: 4096})
		catalog.Add(DiscoveredModel{ID: "nonexistent-model", Provider: "nvidia", MaxContext: 75000, MaxOutput: 6000})

		cfg := &config.Config{
			Providers: map[string]config.ProviderConfig{},
			Agents:    map[string]config.AgentConfig{},
		}
		merged := MergeCatalog(catalog, cfg)

		entries := merged.LookupAll("nonexistent-model")
		if len(entries) != 2 {
			t.Fatalf("expected 2 entries (zen + nvidia), got %d", len(entries))
		}

		providers := make(map[string]int)
		for _, e := range entries {
			providers[e.Provider]++
		}
		if providers["zen"] != 1 {
			t.Errorf("expected 1 zen entry, got %d", providers["zen"])
		}
		if providers["nvidia"] != 1 {
			t.Errorf("expected 1 nvidia entry, got %d", providers["nvidia"])
		}
	})

	t.Run("multiple discovered providers for unregistered model", func(t *testing.T) {
		catalog := NewModelCatalog()
		catalog.Add(DiscoveredModel{ID: "nonexistent-model", Provider: "zen", MaxContext: 100000, MaxOutput: 8192})
		catalog.Add(DiscoveredModel{ID: "nonexistent-model", Provider: "nvidia", MaxContext: 50000, MaxOutput: 4096})
		catalog.Add(DiscoveredModel{ID: "nonexistent-model", Provider: "zai", MaxContext: 200000, MaxOutput: 16384})

		cfg := &config.Config{
			Providers: map[string]config.ProviderConfig{},
			Agents:    map[string]config.AgentConfig{},
		}

		merged := MergeCatalog(catalog, cfg)

		entries := merged.LookupAll("nonexistent-model")
		if len(entries) != 3 {
			t.Fatalf("expected 3 entries (zen + nvidia + zai), got %d", len(entries))
		}

		providers := make(map[string]int)
		for _, e := range entries {
			providers[e.Provider]++
		}
		for _, p := range []string{"zen", "nvidia", "zai"} {
			if providers[p] != 1 {
				t.Errorf("expected 1 %s entry, got %d", p, providers[p])
			}
		}
	})

	t.Run("empty provider in discovered entries preserved", func(t *testing.T) {
		catalog := NewModelCatalog()
		catalog.Add(DiscoveredModel{ID: "test-model", Provider: "", MaxContext: 100000})
		catalog.Add(DiscoveredModel{ID: "test-model", Provider: "zen", MaxContext: 50000})

		cfg := &config.Config{
			Providers: map[string]config.ProviderConfig{},
			Agents:    map[string]config.AgentConfig{},
		}
		merged := MergeCatalog(catalog, cfg)

		entries := merged.LookupAll("test-model")
		if len(entries) != 2 {
			t.Fatalf("expected 2 entries (empty + zen), got %d", len(entries))
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

func TestPricingEntry_HasStandardRate(t *testing.T) {
	tests := []struct {
		name string
		p    PricingEntry
		want bool
	}{
		{"flat input", PricingEntry{InputCostPer1M: 1}, true},
		{"flat output", PricingEntry{OutputCostPer1M: 1}, true},
		{"peak only", PricingEntry{PeakInputCostPer1M: 1}, false},
		{"cached only", PricingEntry{CachedInputCostPer1M: 1}, false},
		{"empty", PricingEntry{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.HasStandardRate(); got != tt.want {
				t.Errorf("HasStandardRate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPricingEntry_JSONRoundTrip(t *testing.T) {
	in := PricingEntry{
		InputCostPer1M:       0.15,
		OutputCostPer1M:      0.60,
		PeakInputCostPer1M:   0.30,
		PeakOutputCostPer1M:  1.20,
		CachedInputCostPer1M: 0.05,
		Currency:             "USD",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out PricingEntry
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

// TestMergeCatalog_StaticPricingBecomesCatalogPricing pins the Phase 007
// bridge: static registry pricing (incl. peak/cached) must surface as
// DiscoveredModel.Pricing so billing and the cost guard see it without an
// attached external feed.
func TestMergeCatalog_StaticPricingBecomesCatalogPricing(t *testing.T) {
	const id = "cost-model-test-merge-static"
	config.ModelRegistry[id] = config.ModelEntry{
		Provider: "deepseek",
		Pricing: config.PricingOverride{
			InputCostPer1M:       0.15,
			OutputCostPer1M:      0.60,
			PeakInputCostPer1M:   0.30,
			PeakOutputCostPer1M:  1.20,
			CachedInputCostPer1M: 0.05,
		},
	}
	t.Cleanup(func() { delete(config.ModelRegistry, id) })

	merged := MergeCatalog(NewModelCatalog(), &config.Config{
		Providers: map[string]config.ProviderConfig{},
		Agents:    map[string]config.AgentConfig{},
	})
	dm, ok := merged.Lookup(id)
	if !ok {
		t.Fatal("merged catalog missing model")
	}
	if dm.Pricing == nil {
		t.Fatal("static pricing must surface as catalog pricing")
	}
	if !dm.Pricing.HasPeak() || dm.Pricing.PeakInputCostPer1M != 0.30 || dm.Pricing.PeakOutputCostPer1M != 1.20 {
		t.Errorf("peak fields lost: %+v", dm.Pricing)
	}
	if dm.Pricing.CachedInputCostPer1M != 0.05 || !dm.Pricing.HasStandardRate() {
		t.Errorf("standard/cached fields lost: %+v", dm.Pricing)
	}
}

// TestAttachPricing_PreservesStaticPeak pins that an attached baseline-only
// feed (OpenRouter) never wipes the static peak/cached dimensions.
func TestAttachPricing_PreservesStaticPeak(t *testing.T) {
	const id = "cost-model-test-attach-peak"
	config.ModelRegistry[id] = config.ModelEntry{
		Provider: "p",
		Pricing: config.PricingOverride{
			InputCostPer1M:       0.15,
			OutputCostPer1M:      0.60,
			PeakInputCostPer1M:   0.30,
			PeakOutputCostPer1M:  1.20,
			CachedInputCostPer1M: 0.05,
		},
	}
	t.Cleanup(func() { delete(config.ModelRegistry, id) })

	merged := MergeCatalog(NewModelCatalog(), &config.Config{
		Providers: map[string]config.ProviderConfig{},
		Agents:    map[string]config.AgentConfig{},
	})
	merged.AttachPricing(map[string]PricingEntry{
		id: {InputCostPer1M: 0.99, OutputCostPer1M: 1.99, Currency: "USD"},
	})
	dm, ok := merged.Lookup(id)
	if !ok || dm.Pricing == nil {
		t.Fatal("model missing after attach")
	}
	if dm.Pricing.InputCostPer1M != 0.99 {
		t.Errorf("attached baseline must win: %+v", dm.Pricing)
	}
	if dm.Pricing.PeakInputCostPer1M != 0.30 || dm.Pricing.PeakOutputCostPer1M != 1.20 || dm.Pricing.CachedInputCostPer1M != 0.05 {
		t.Errorf("static peak/cached wiped by attached feed: %+v", dm.Pricing)
	}
}

// TestMergeCatalog_AgentOverrideCarriesStaticPricing pins the agent-override
// merge path (max_context/max_output overrides) carrying static pricing —
// a regression here silently unpriced billing for overridden models.
func TestMergeCatalog_AgentOverrideCarriesStaticPricing(t *testing.T) {
	const id = "cost-model-test-override-pricing"
	config.ModelRegistry[id] = config.ModelEntry{
		Provider: "deepseek",
		Pricing: config.PricingOverride{
			InputCostPer1M:      0.15,
			OutputCostPer1M:     0.60,
			PeakInputCostPer1M:  0.30,
			PeakOutputCostPer1M: 1.20,
		},
	}
	t.Cleanup(func() { delete(config.ModelRegistry, id) })

	agent := config.AgentConfig{}
	agent.Models = []config.AgentModel{{
		Model:      id,
		Provider:   "deepseek",
		MaxContext: 123456,
		MaxOutput:  4096,
	}}
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{},
		Agents:    map[string]config.AgentConfig{"agent": agent},
	}
	merged := MergeCatalog(NewModelCatalog(), cfg)
	found := false
	for _, m := range merged.AllModels() {
		if m.ID == id && m.Pricing != nil && m.Pricing.HasPeak() {
			found = true
		}
	}
	if !found {
		t.Fatal("agent-override merge path lost the static pricing")
	}
}
