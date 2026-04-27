package routing

import (
	"math"
	"strings"

	"nenya/internal/discovery"
)

// capabilityOverlap counts how many capability flags are both present and true
// in the source and candidate models.
func capabilityOverlap(source, candidate *discovery.ModelMetadata) int {
	if source == nil || candidate == nil {
		return 0
	}
	overlap := 0
	if source.SupportsVision && candidate.SupportsVision {
		overlap++
	}
	if source.SupportsToolCalls && candidate.SupportsToolCalls {
		overlap++
	}
	if source.SupportsReasoning && candidate.SupportsReasoning {
		overlap++
	}
	if source.SupportsContentArrays && candidate.SupportsContentArrays {
		overlap++
	}
	if source.SupportsStreamOptions && candidate.SupportsStreamOptions {
		overlap++
	}
	if source.SupportsAutoToolChoice && candidate.SupportsAutoToolChoice {
		overlap++
	}
	return overlap
}

// sameFamily returns true when both models define a non-empty Family string
// and those strings match.
func sameFamily(source, candidate *discovery.ModelMetadata) bool {
	if source == nil || candidate == nil {
		return false
	}
	sourceFamily := strings.TrimSpace(source.Family)
	candidateFamily := strings.TrimSpace(candidate.Family)
	if sourceFamily == "" || candidateFamily == "" {
		return false
	}
	return sourceFamily == candidateFamily
}

// absInt returns the absolute value of an integer
func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// eloDiff returns the absolute difference between two Elo scores.
// If either Elo is nil, returns 0.
func eloDiff(sourceElo, candidateElo *float64) float64 {
	if sourceElo == nil || candidateElo == nil {
		return 0
	}
	return math.Abs(*candidateElo - *sourceElo)
}

// rankDiff returns the absolute difference between two rank values.
// If either rank is nil, returns 0.
func rankDiff(sourceRank, candidateRank *int) int {
	if sourceRank == nil || candidateRank == nil {
		return 0
	}
	return absInt(*candidateRank - *sourceRank)
}
