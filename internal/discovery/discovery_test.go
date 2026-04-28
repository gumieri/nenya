package discovery

import (
	"testing"
)

func TestModelCatalog_MultiProviderLookup(t *testing.T) {
	c := NewModelCatalog()

	c.Add(DiscoveredModel{ID: "deepseek-v4-flash", Provider: "deepseek", MaxContext: 100000, MaxOutput: 393216})
	c.Add(DiscoveredModel{ID: "deepseek-v4-flash", Provider: "nvidia", MaxContext: 50000, MaxOutput: 16384})
	c.Add(DiscoveredModel{ID: "deepseek-v4-pro", Provider: "deepseek", MaxContext: 1000000, MaxOutput: 393216})

	t.Run("LookupAll returns all providers", func(t *testing.T) {
		results := c.LookupAll("deepseek-v4-flash")
		if len(results) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(results))
		}
		providers := make(map[string]bool)
		for _, m := range results {
			providers[m.Provider] = true
		}
		if !providers["deepseek"] || !providers["nvidia"] {
			t.Fatalf("expected providers deepseek and nvidia, got %v", providers)
		}
	})

	t.Run("Lookup returns first entry for backward compat", func(t *testing.T) {
		m, ok := c.Lookup("deepseek-v4-flash")
		if !ok {
			t.Fatal("expected lookup to succeed")
		}
		if m.ID != "deepseek-v4-flash" {
			t.Fatalf("expected deepseek-v4-flash, got %s", m.ID)
		}
	})

	t.Run("LookupAll single provider", func(t *testing.T) {
		results := c.LookupAll("deepseek-v4-pro")
		if len(results) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(results))
		}
		if results[0].Provider != "deepseek" {
			t.Fatalf("expected deepseek, got %s", results[0].Provider)
		}
	})

	t.Run("LookupAll missing model returns nil", func(t *testing.T) {
		results := c.LookupAll("nonexistent-model")
		if len(results) != 0 {
			t.Fatalf("expected 0 entries, got %d", len(results))
		}
	})

	t.Run("Lookup missing model returns false", func(t *testing.T) {
		_, ok := c.Lookup("nonexistent-model")
		if ok {
			t.Fatal("expected false for missing model")
		}
	})
}

func TestModelCatalog_AllModels_FlattensMultiProvider(t *testing.T) {
	c := NewModelCatalog()
	c.Add(DiscoveredModel{ID: "model-a", Provider: "p1"})
	c.Add(DiscoveredModel{ID: "model-a", Provider: "p2"})
	c.Add(DiscoveredModel{ID: "model-b", Provider: "p1"})

	all := c.AllModels()
	if len(all) != 3 {
		t.Fatalf("expected 3 entries (2 for model-a + 1 for model-b), got %d", len(all))
	}
}

func TestModelCatalog_ModelsForProvider_Unchanged(t *testing.T) {
	c := NewModelCatalog()
	c.Add(DiscoveredModel{ID: "model-a", Provider: "p1"})
	c.Add(DiscoveredModel{ID: "model-a", Provider: "p2"})
	c.Add(DiscoveredModel{ID: "model-b", Provider: "p1"})

	models := c.ModelsForProvider("p1")
	if len(models) != 2 {
		t.Fatalf("expected 2 models for p1, got %d", len(models))
	}

	models = c.ModelsForProvider("p2")
	if len(models) != 1 {
		t.Fatalf("expected 1 model for p2, got %d", len(models))
	}
}

func TestModelCatalog_AttachPricing_AllProviders(t *testing.T) {
	c := NewModelCatalog()
	c.Add(DiscoveredModel{ID: "model-a", Provider: "p1", MaxContext: 1000})
	c.Add(DiscoveredModel{ID: "model-a", Provider: "p2", MaxContext: 2000})

	pricing := map[string]PricingEntry{
		"model-a": {InputCostPer1M: 1.0, OutputCostPer1M: 2.0},
	}
	c.AttachPricing(pricing)

	for _, m := range c.LookupAll("model-a") {
		if m.Pricing == nil {
			t.Fatalf("expected pricing for provider %s", m.Provider)
		}
		if m.Pricing.InputCostPer1M != 1.0 {
			t.Fatalf("expected input cost 1.0, got %f", m.Pricing.InputCostPer1M)
		}
	}
}
