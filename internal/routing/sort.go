package routing

import (
	"math"
	"sort"

	"nenya/internal/discovery"
	"nenya/internal/infra"
)

type SortOptions struct {
	LatencyWeight float64
	CostWeight    float64
	RequestCaps   RequestCapabilities
}

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

	if opts.LatencyWeight == 0 && opts.CostWeight == 0 && !hasCatalogMetadata(catalog) {
		return targets
	}

	var latencies []float64
	var costs []float64

	for _, t := range targets {
		if latencyTracker != nil {
			if lat, ok := latencyTracker.Get(t.Model, t.Provider); ok {
				latencies = append(latencies, lat.MedianMs)
			}
		}
		if costTracker != nil {
			if c := costTracker.GetCost(t.Model); c > 0 {
				costs = append(costs, float64(c))
			}
		}
	}

	minLat, maxLat := minMax(latencies)
	minCost, maxCost := minMax(costs)

	sorted := make([]UpstreamTarget, len(targets))
	copy(sorted, targets)

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

func minMax(values []float64) (min, max float64) {
	if len(values) == 0 {
		return 0, 1
	}
	min = values[0]
	max = values[0]
	for _, v := range values[1:] {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	if min == max {
		return 0, 1
	}
	return min, max
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
		if c := costTracker.GetCost(target.Model); c > 0 {
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

func hasCatalogMetadata(catalog *discovery.ModelCatalog) bool {
	if catalog == nil {
		return false
	}
	for _, m := range catalog.AllModels() {
		if m.Metadata != nil && (m.Metadata.ScoreBonus != 0 ||
			m.Metadata.SupportsToolCalls || m.Metadata.SupportsReasoning ||
			m.Metadata.SupportsVision || m.Metadata.SupportsContentArrays) {
			return true
		}
	}
	return false
}
