package routing

import (
	"math"
	"sort"

	"nenya/internal/discovery"
	"nenya/internal/infra"
)

// SortOptions configures the weights used by balanced sorting strategies.
type SortOptions struct {
	LatencyWeight float64
	CostWeight    float64
	RequestCaps   RequestCapabilities
}

// RequestCapabilities describes the features detected in an incoming request
// payload (tool calls, reasoning, vision, content arrays).
type RequestCapabilities struct {
	HasToolCalls  bool
	HasReasoning  bool
	HasVision     bool
	HasContentArr bool
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

	collectMinMax := func(fn func(t UpstreamTarget) (float64, bool)) (min, max float64) {
		min, max = -1, -1
		for _, t := range sorted {
			if v, ok := fn(t); ok {
				if min < 0 || v < min {
					min = v
				}
				if max < 0 || v > max {
					max = v
				}
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

	minLat, maxLat := collectMinMax(func(t UpstreamTarget) (float64, bool) {
		if latencyTracker == nil {
			return 0, false
		}
		lat, ok := latencyTracker.Get(t.Model, t.Provider)
		return lat.MedianMs, ok
	})
	minCost, maxCost := collectMinMax(func(t UpstreamTarget) (float64, bool) {
		if costTracker == nil {
			return 0, false
		}
		c := costTracker.GetCostMicroUSD(t.Model)
		return float64(c), c > 0
	})

	sort.SliceStable(sorted, func(i, j int) bool {
		scoreI := calculateScore(sorted[i], latencyTracker, costTracker, catalog, minLat, maxLat, minCost, maxCost, opts)
		scoreJ := calculateScore(sorted[j], latencyTracker, costTracker, catalog, minLat, maxLat, minCost, maxCost, opts)

		if scoreI != scoreJ {
			return scoreI > scoreJ
		}

		if latencyTracker != nil {
			latI, okI := latencyTracker.Get(sorted[i].Model, sorted[i].Provider)
			latJ, okJ := latencyTracker.Get(sorted[j].Model, sorted[j].Provider)
			if okI && okJ {
				return latI.MedianMs < latJ.MedianMs
			}
			if okI {
				return true
			}
			if okJ {
				return false
			}
		}
		return false
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

	scoreBonus := 0.0
	if catalog != nil {
		if meta, ok := catalog.Lookup(target.Model); ok && meta.Metadata != nil {
			scoreBonus += meta.Metadata.ScoreBonus
			scoreBonus += capabilityBoost(meta.Metadata, opts.RequestCaps)
		}
	}

	score := (latencyNorm * opts.LatencyWeight) - (costNorm * opts.CostWeight) + scoreBonus
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0
	}
	return score
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
