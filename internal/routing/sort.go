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

// collectMinMax returns the min and max over the observed values; ok is true
// only when at least two distinct values were observed, i.e. a real
// normalization range exists (a degenerate or empty observation set keeps the
// caller at the neutral score instead of normalizing against a sentinel).
func collectMinMax(targets []UpstreamTarget, fn func(t UpstreamTarget) (float64, bool)) (min, max float64, ok bool) {
	min, max = -1, -1
	for _, t := range targets {
		v, valid := fn(t)
		if !valid {
			continue
		}
		if min < 0 || v < min {
			min = v
		}
		if max < 0 || v > max {
			max = v
		}
	}
	return min, max, min >= 0 && max > min
}

// getLatencyRange returns the min/max median latency across targets with
// recorded data; ok is false when no real normalization range exists.
func getLatencyRange(targets []UpstreamTarget, latencyTracker *infra.LatencyTracker) (min, max float64, ok bool) {
	return collectMinMax(targets, func(t UpstreamTarget) (float64, bool) {
		if latencyTracker == nil {
			return 0, false
		}
		lat, found := latencyTracker.Get(t.Model, t.Provider)
		if !found {
			return 0, false
		}
		return lat.MedianMs, true
	})
}

// getCostRange returns the min/max micro-USD cost across targets with
// recorded data; ok is false when no real normalization range exists.
func getCostRange(targets []UpstreamTarget, costTracker *infra.CostTracker) (min, max float64, ok bool) {
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

	minLat, maxLat, latRangeOK := getLatencyRange(sorted, latencyTracker)
	minCost, maxCost, costRangeOK := getCostRange(sorted, costTracker)

	in := scoreInput{
		latencyTracker: latencyTracker,
		costTracker:    costTracker,
		catalog:        catalog,
		minLat:         minLat,
		maxLat:         maxLat,
		latRangeOK:     latRangeOK,
		minCost:        minCost,
		maxCost:        maxCost,
		costRangeOK:    costRangeOK,
		opts:           opts,
	}

	sort.SliceStable(sorted, func(i, j int) bool {
		scoreI := calculateScore(sorted[i], in)
		scoreJ := calculateScore(sorted[j], in)

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

// scoreInput groups the trackers, catalog, precomputed normalization ranges,
// and options shared by every calculateScore call within one sort.
type scoreInput struct {
	latencyTracker   *infra.LatencyTracker
	costTracker      *infra.CostTracker
	catalog          *discovery.ModelCatalog
	minLat, maxLat   float64
	latRangeOK       bool
	minCost, maxCost float64
	costRangeOK      bool
	opts             SortOptions
}

// calculateScore computes the balanced ranking score for one target: weighted
// normalized latency (lower median is better) minus weighted normalized cost
// and billing penalty, plus capability/score bonuses. Dimensions without data
// — or with a degenerate single-value range — contribute the neutral 0.5.
func calculateScore(target UpstreamTarget, in scoreInput) float64 {
	latencyNorm := 0.5
	if in.latRangeOK && in.latencyTracker != nil && in.opts.LatencyWeight > 0 {
		if lat, ok := in.latencyTracker.Get(target.Model, target.Provider); ok {
			latencyNorm = (in.maxLat - lat.MedianMs) / (in.maxLat - in.minLat)
		}
	}

	costNorm := 0.5
	if in.costRangeOK && in.costTracker != nil && in.opts.CostWeight > 0 {
		if c := in.costTracker.GetCostMicroUSD(target.Model); c > 0 {
			costNorm = (float64(c) - in.minCost) / (in.maxCost - in.minCost)
		}
	}

	billingWeight := getBillingWeight(in.opts, target.Provider)

	scoreBonus := computeScoreBonus(target, in.opts, in.catalog)

	score := (latencyNorm * in.opts.LatencyWeight) - (costNorm * in.opts.CostWeight) - (costNorm * billingWeight) + scoreBonus
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
		if dm, ok := catalog.Lookup(model); ok && dm.Pricing != nil && dm.Pricing.HasStandardRate() {
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
