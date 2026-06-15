package routing

import (
	"math"
	"sort"

	"github.com/nenya/config"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/infra"
)

const (
	BillingEconomyDefaultScale  = 1.5
	BillingBalancedDefaultScale = 1.0
	BillingQualityDefaultScale  = 0.0
)

type BillingMode string

const (
	BillingEconomy  BillingMode = "economy"
	BillingBalanced BillingMode = "balanced"
	BillingQuality  BillingMode = "quality"
)

// SortOptions configures the weights used by balanced sorting strategies.
type SortOptions struct {
	LatencyWeight     float64
	CostWeight        float64
	BillingMode       BillingMode
	BillingEconomy    float64
	BillingQuality    float64
	RequestCaps       RequestCapabilities
	BillingModel      map[string]string
	BillingFreeOnly   map[string]bool
	BillingFreeModels map[string][]string // static config for scoring bonuses only; runtime exhaustion uses BillingTracker.IsExhausted
}

// RequestCapabilities describes the features detected in an incoming request
// payload (tool calls, reasoning, vision, content arrays).
type RequestCapabilities struct {
	HasToolCalls  bool
	HasReasoning  bool
	HasVision     bool
	HasContentArr bool
}

func collectMinMax(targets []UpstreamTarget, fn func(t UpstreamTarget) (float64, bool)) (min, max float64) {
	min, max = -1, -1
	for _, t := range targets {
		v, ok := fn(t)
		if !ok {
			continue
		}
		if min < 0 || v < min {
			min = v
		}
		if max < 0 || v > max {
			max = v
		}
	}
	if min < 0 {
		return 0, 1
	}
	if min == max {
		return 0, 1
	}
	return min, max
}

func getLatencyRange(targets []UpstreamTarget, latencyTracker *infra.LatencyTracker) (min, max float64) {
	return collectMinMax(targets, func(t UpstreamTarget) (float64, bool) {
		if latencyTracker == nil {
			return 0, false
		}
		lat, ok := latencyTracker.Get(t.Model, t.Provider)
		return lat.MedianMs, ok
	})
}

func getCostRange(targets []UpstreamTarget, costTracker *infra.CostTracker) (min, max float64) {
	return collectMinMax(targets, func(t UpstreamTarget) (float64, bool) {
		if costTracker == nil {
			return 0, false
		}
		c := costTracker.GetCostMicroUSD(t.Model)
		return float64(c), c > 0
	})
}

func compareByLatency(i, j UpstreamTarget, latencyTracker *infra.LatencyTracker) bool {
	latI, okI := latencyTracker.Get(i.Model, i.Provider)
	latJ, okJ := latencyTracker.Get(j.Model, j.Provider)
	if okI && okJ {
		return latI.MedianMs < latJ.MedianMs
	}
	if okI {
		return true
	}
	if okJ {
		return false
	}
	return false
}

func SortTargetsByBalanced(targets []UpstreamTarget, latencyTracker *infra.LatencyTracker, costTracker *infra.CostTracker, catalog *discovery.ModelCatalog, opts SortOptions) []UpstreamTarget {
	if len(targets) <= 1 {
		return targets
	}
	if opts.LatencyWeight == 0 && opts.CostWeight == 0 && (catalog == nil || !catalog.HasMetadata()) {
		return targets
	}

	sorted := make([]UpstreamTarget, len(targets))
	copy(sorted, targets)

	minLat, maxLat := getLatencyRange(sorted, latencyTracker)
	minCost, maxCost := getCostRange(sorted, costTracker)

	sort.SliceStable(sorted, func(i, j int) bool {
		scoreI := calculateScore(sorted[i], latencyTracker, costTracker, catalog, minLat, maxLat, minCost, maxCost, opts)
		scoreJ := calculateScore(sorted[j], latencyTracker, costTracker, catalog, minLat, maxLat, minCost, maxCost, opts)

		if scoreI != scoreJ {
			return scoreI > scoreJ
		}

		if latencyTracker == nil {
			return false
		}
		return compareByLatency(sorted[i], sorted[j], latencyTracker)
	})

	return sorted
}

func calculateScore(target UpstreamTarget, latencyTracker *infra.LatencyTracker, costTracker *infra.CostTracker, catalog *discovery.ModelCatalog, minLat, maxLat, minCost, maxCost float64, opts SortOptions) float64 {
	latencyNorm := 0.5
	if latencyTracker != nil && opts.LatencyWeight > 0 {
		if lat, ok := latencyTracker.Get(target.Model, target.Provider); ok {
			if maxLat > minLat {
				latencyNorm = (maxLat - lat.MedianMs) / (maxLat - minLat)
			}
		}
	}

	costNorm := 0.5
	if costTracker != nil && opts.CostWeight > 0 {
		if c := costTracker.GetCostMicroUSD(target.Model); c > 0 {
			fc := float64(c)
			if maxCost > minCost {
				costNorm = (fc - minCost) / (maxCost - minCost)
			}
		}
	}

	billingWeight := getBillingWeight(opts, target.Provider)

	scoreBonus := computeScoreBonus(target, opts, catalog)

	score := (latencyNorm * opts.LatencyWeight) - (costNorm * opts.CostWeight) - (costNorm * billingWeight) + scoreBonus
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0
	}
	return score
}

func computeScoreBonus(target UpstreamTarget, opts SortOptions, catalog *discovery.ModelCatalog) float64 {
	bonus := 0.0
	if catalog != nil {
		if meta, ok := catalog.Lookup(target.Model); ok && meta.Metadata != nil {
			bonus += meta.Metadata.ScoreBonus
			bonus += capabilityBoost(meta.Metadata, opts.RequestCaps)
		}
	}

	if opts.BillingFreeOnly[target.Provider] {
		bonus += 0.4
	} else if bm, ok := opts.BillingModel[target.Provider]; ok && bm == string(config.BillingMixed) {
		if isModelFreeInProvider(target.Model, target.Provider, opts, catalog) {
			bonus += 0.4
		}
	}
	return bonus
}

func isModelFreeInProvider(model, provider string, opts SortOptions, catalog *discovery.ModelCatalog) bool {
	// Priority: explicit config list > name suffix heuristic > catalog pricing.
	// This priority order is intentionally optimistic (favors treating models
	// as free) because the +0.4 bonus is mild. Contrast with
	// isPaidModelOnFreeOnlyProvider which uses pricing before name suffix
	// (conservative for filtering, where false positives are costly).
	if freeModels, ok := opts.BillingFreeModels[provider]; ok {
		for _, fm := range freeModels {
			if fm == model {
				return true
			}
		}
	}

	if isFreeModelName(model) {
		return true
	}

	if catalog != nil {
		if dm, ok := catalog.Lookup(model); ok && dm.Pricing != nil && !dm.Pricing.IsZero() {
			if dm.Pricing.InputCostPer1M <= DefaultFreeOnlyInputPriceThreshold &&
				dm.Pricing.OutputCostPer1M <= DefaultFreeOnlyInputPriceThreshold {
				return true
			}
		}
	}

	return false
}

func getBillingWeight(opts SortOptions, provider string) float64 {
	billingModel := BillingBalanced
	if bm, ok := opts.BillingModel[provider]; ok {
		billingModel = BillingMode(bm)
	}
	if opts.BillingFreeOnly[provider] {
		billingModel = BillingEconomy
	}
	switch billingModel {
	case BillingEconomy:
		return opts.BillingEconomy
	case BillingQuality:
		return opts.BillingQuality
	default:
		return BillingBalancedDefaultScale
	}
}

func capabilityBoost(meta *discovery.ModelMetadata, caps RequestCapabilities) float64 {
	if meta == nil {
		return 0
	}
	boost := 0.0
	matchCount := 0
	totalCaps := 0

	if caps.HasToolCalls {
		totalCaps++
		if meta.SupportsToolCalls {
			boost += 0.1
			matchCount++
		}
	}
	if caps.HasReasoning {
		totalCaps++
		if meta.SupportsReasoning {
			boost += 0.1
			matchCount++
		}
	}
	if caps.HasVision {
		totalCaps++
		if meta.SupportsVision {
			boost += 0.1
			matchCount++
		}
	}
	if caps.HasContentArr {
		totalCaps++
		if meta.SupportsContentArrays {
			boost += 0.1
			matchCount++
		}
	}

	if totalCaps > 0 && matchCount == 0 {
		boost -= 0.1 * float64(totalCaps)
	}
	return boost
}
