package routing

import (
	"testing"
	"time"

	"nenya/internal/discovery"
	"nenya/internal/infra"
)

func TestSortTargetsByBalanced_LatencyOnly(t *testing.T) {
	tracker := infra.NewLatencyTracker()
	tracker.Record("fast-model", "p1", 100*time.Millisecond)
	tracker.Record("fast-model", "p1", 120*time.Millisecond)
	tracker.Record("slow-model", "p1", 500*time.Millisecond)
	tracker.Record("slow-model", "p1", 480*time.Millisecond)

	targets := []UpstreamTarget{
		{Model: "slow-model", Provider: "p1", URL: "http://slow"},
		{Model: "fast-model", Provider: "p1", URL: "http://fast"},
	}

	sorted := SortTargetsByBalanced(targets, tracker, nil, nil, SortOptions{
		LatencyWeight: 1.0,
		CostWeight:    0.0,
	})

	if len(sorted) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(sorted))
	}
	if sorted[0].Model != "fast-model" {
		t.Errorf("expected fast-model first, got %s", sorted[0].Model)
	}
}

func TestSortTargetsByBalanced_CostOnly(t *testing.T) {
	ct := infra.NewCostTracker()
	ct.RecordUsage("cheap-model", 100)
	ct.RecordUsage("expensive-model", 10000)

	targets := []UpstreamTarget{
		{Model: "expensive-model", Provider: "p1", URL: "http://exp"},
		{Model: "cheap-model", Provider: "p1", URL: "http://cheap"},
	}

	sorted := SortTargetsByBalanced(targets, nil, ct, nil, SortOptions{
		LatencyWeight: 0.0,
		CostWeight:    1.0,
	})

	if sorted[0].Model != "cheap-model" {
		t.Errorf("expected cheap-model first, got %s", sorted[0].Model)
	}
}

func TestSortTargetsByBalanced_ScoreBonus(t *testing.T) {
	catalog := discovery.NewModelCatalog()
	catalog.Add(discovery.DiscoveredModel{
		ID: "boosted-model",
		Metadata: &discovery.ModelMetadata{
			ScoreBonus: 1.0,
		},
	})
	catalog.Add(discovery.DiscoveredModel{
		ID: "normal-model",
	})

	targets := []UpstreamTarget{
		{Model: "normal-model", Provider: "p1", URL: "http://normal"},
		{Model: "boosted-model", Provider: "p1", URL: "http://boosted"},
	}

	sorted := SortTargetsByBalanced(targets, nil, nil, catalog, SortOptions{
		LatencyWeight: 0.0,
		CostWeight:    0.0,
	})

	if sorted[0].Model != "boosted-model" {
		t.Errorf("expected boosted-model first, got %s", sorted[0].Model)
	}
}

func TestSortTargetsByBalanced_CapabilityBoost(t *testing.T) {
	catalog := discovery.NewModelCatalog()
	catalog.Add(discovery.DiscoveredModel{
		ID: "tool-model",
		Metadata: &discovery.ModelMetadata{
			SupportsToolCalls: true,
		},
	})
	catalog.Add(discovery.DiscoveredModel{
		ID: "no-tool-model",
	})

	targets := []UpstreamTarget{
		{Model: "no-tool-model", Provider: "p1", URL: "http://notool"},
		{Model: "tool-model", Provider: "p1", URL: "http://tool"},
	}

	sorted := SortTargetsByBalanced(targets, nil, nil, catalog, SortOptions{
		LatencyWeight: 0.0,
		CostWeight:    0.0,
		RequestCaps: RequestCapabilities{
			HasToolCalls: true,
		},
	})

	if sorted[0].Model != "tool-model" {
		t.Errorf("expected tool-model first, got %s", sorted[0].Model)
	}
}

func TestSortTargetsByBalanced_CapabilityMismatch_Penalty(t *testing.T) {
	catalog := discovery.NewModelCatalog()
	catalog.Add(discovery.DiscoveredModel{
		ID: "no-tool-model",
		Metadata: &discovery.ModelMetadata{
			SupportsReasoning: true,
		},
	})
	catalog.Add(discovery.DiscoveredModel{
		ID: "basic-model",
	})

	targets := []UpstreamTarget{
		{Model: "basic-model", Provider: "p1", URL: "http://basic"},
		{Model: "no-tool-model", Provider: "p1", URL: "http://notool"},
	}

	sorted := SortTargetsByBalanced(targets, nil, nil, catalog, SortOptions{
		LatencyWeight: 0.0,
		CostWeight:    0.0,
		RequestCaps: RequestCapabilities{
			HasToolCalls: true,
		},
	})

	if sorted[0].Model != "basic-model" {
		t.Errorf("expected basic-model first (no-tool-model should be penalized), got %s", sorted[0].Model)
	}
}

func TestSortTargetsByBalanced_ZeroWeights_NoOp(t *testing.T) {
	tracker := infra.NewLatencyTracker()
	tracker.Record("slow-model", "p1", 500*time.Millisecond)
	tracker.Record("fast-model", "p1", 100*time.Millisecond)

	targets := []UpstreamTarget{
		{Model: "slow-model", Provider: "p1", URL: "http://slow"},
		{Model: "fast-model", Provider: "p1", URL: "http://fast"},
	}

	sorted := SortTargetsByBalanced(targets, tracker, nil, nil, SortOptions{
		LatencyWeight: 0.0,
		CostWeight:    0.0,
	})

	if sorted[0].Model != "slow-model" {
		t.Errorf("expected original order preserved with zero weights, got %s first", sorted[0].Model)
	}
}

func TestSortTargetsByBalanced_SingleTarget(t *testing.T) {
	targets := []UpstreamTarget{
		{Model: "only-model", Provider: "p1", URL: "http://only"},
	}

	sorted := SortTargetsByBalanced(targets, nil, nil, nil, SortOptions{
		LatencyWeight: 1.0,
		CostWeight:    1.0,
	})

	if len(sorted) != 1 || sorted[0].Model != "only-model" {
		t.Errorf("single target should pass through unchanged")
	}
}

func TestSortTargetsByBalanced_EmptyTargets(t *testing.T) {
	sorted := SortTargetsByBalanced(nil, nil, nil, nil, SortOptions{
		LatencyWeight: 1.0,
		CostWeight:    1.0,
	})

	if len(sorted) != 0 {
		t.Errorf("empty targets should return empty")
	}
}

func TestSortTargetsByBalanced_NoData_DefaultsToNeutral(t *testing.T) {
	targets := []UpstreamTarget{
		{Model: "model-a", Provider: "p1", URL: "http://a"},
		{Model: "model-b", Provider: "p1", URL: "http://b"},
	}

	sorted := SortTargetsByBalanced(targets, nil, nil, nil, SortOptions{
		LatencyWeight: 1.0,
		CostWeight:    1.0,
	})

	if len(sorted) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(sorted))
	}
}

func TestMinMax(t *testing.T) {
	tests := []struct {
		name     string
		values   []float64
		wantMin  float64
		wantMax  float64
	}{
		{"empty", nil, 0, 1},
		{"single", []float64{5.0}, 0, 1},
		{"normal", []float64{1.0, 5.0, 3.0}, 1.0, 5.0},
		{"all_same", []float64{3.0, 3.0, 3.0}, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			min, max := minMax(tt.values)
			if min != tt.wantMin || max != tt.wantMax {
				t.Errorf("minMax(%v) = (%v, %v), want (%v, %v)", tt.values, min, max, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestCapabilityBoost(t *testing.T) {
	tests := []struct {
		name string
		meta *discovery.ModelMetadata
		caps RequestCapabilities
		want float64
	}{
		{
			"nil meta",
			nil,
			RequestCapabilities{HasToolCalls: true},
			0,
		},
		{
			"full match",
			&discovery.ModelMetadata{SupportsToolCalls: true, SupportsReasoning: true},
			RequestCapabilities{HasToolCalls: true, HasReasoning: true},
			0.2,
		},
		{
			"no caps requested",
			&discovery.ModelMetadata{SupportsToolCalls: true},
			RequestCapabilities{},
			0,
		},
		{
			"mismatch penalty",
			&discovery.ModelMetadata{},
			RequestCapabilities{HasToolCalls: true, HasReasoning: true},
			-0.2,
		},
		{
			"partial match",
			&discovery.ModelMetadata{SupportsToolCalls: true},
			RequestCapabilities{HasToolCalls: true, HasReasoning: true},
			0.1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := capabilityBoost(tt.meta, tt.caps)
			if got != tt.want {
				t.Errorf("capabilityBoost() = %v, want %v", got, tt.want)
			}
		})
	}
}
