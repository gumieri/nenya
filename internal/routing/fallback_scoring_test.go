package routing

import (
	"math"
	"testing"

	"nenya/internal/discovery"
)

func TestCapabilityOverlap(t *testing.T) {
	tests := []struct {
		name     string
		source   *discovery.ModelMetadata
		candidate *discovery.ModelMetadata
		want     int
	}{
		{
			name:     "both nil",
			source:   nil,
			candidate: nil,
			want:     0,
		},
		{
			name:     "source nil",
			source:   nil,
			candidate: &discovery.ModelMetadata{SupportsVision: true},
			want:     0,
		},
		{
			name:     "candidate nil",
			source:   &discovery.ModelMetadata{SupportsVision: true},
			candidate: nil,
			want:     0,
		},
		{
			name:     "no overlap",
			source:   &discovery.ModelMetadata{SupportsVision: true},
			candidate: &discovery.ModelMetadata{SupportsToolCalls: true},
			want:     0,
		},
		{
			name:     "single overlap",
			source:   &discovery.ModelMetadata{SupportsVision: true, SupportsToolCalls: true},
			candidate: &discovery.ModelMetadata{SupportsVision: true},
			want:     1,
		},
		{
			name:     "multiple overlap",
			source:   &discovery.ModelMetadata{SupportsVision: true, SupportsToolCalls: true, SupportsReasoning: true},
			candidate: &discovery.ModelMetadata{SupportsVision: true, SupportsToolCalls: true},
			want:     2,
		},
		{
			name:     "all overlap",
			source:   &discovery.ModelMetadata{SupportsVision: true, SupportsToolCalls: true, SupportsReasoning: true, SupportsContentArrays: true, SupportsStreamOptions: true, SupportsAutoToolChoice: true},
			candidate: &discovery.ModelMetadata{SupportsVision: true, SupportsToolCalls: true, SupportsReasoning: true, SupportsContentArrays: true, SupportsStreamOptions: true, SupportsAutoToolChoice: true},
			want:     6,
		},
		{
			name:     "partial overlap",
			source:   &discovery.ModelMetadata{SupportsVision: true, SupportsToolCalls: true, SupportsReasoning: true},
			candidate: &discovery.ModelMetadata{SupportsVision: true, SupportsReasoning: true},
			want:     2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := capabilityOverlap(tt.source, tt.candidate)
			if got != tt.want {
				t.Errorf("capabilityOverlap() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSameFamily(t *testing.T) {
	tests := []struct {
		name      string
		source    *discovery.ModelMetadata
		candidate *discovery.ModelMetadata
		want      bool
	}{
		{
			name:      "both nil",
			source:    nil,
			candidate: nil,
			want:      false,
		},
		{
			name:      "source nil",
			source:    nil,
			candidate: &discovery.ModelMetadata{Family: "claude"},
			want:      false,
		},
		{
			name:      "candidate nil",
			source:    &discovery.ModelMetadata{Family: "claude"},
			candidate: nil,
			want:      false,
		},
		{
			name:      "both empty family",
			source:    &discovery.ModelMetadata{Family: ""},
			candidate: &discovery.ModelMetadata{Family: ""},
			want:      false,
		},
		{
			name:      "source empty family",
			source:    &discovery.ModelMetadata{Family: ""},
			candidate: &discovery.ModelMetadata{Family: "claude"},
			want:      false,
		},
		{
			name:      "candidate empty family",
			source:    &discovery.ModelMetadata{Family: "claude"},
			candidate: &discovery.ModelMetadata{Family: ""},
			want:      false,
		},
		{
			name:      "different families",
			source:    &discovery.ModelMetadata{Family: "claude"},
			candidate: &discovery.ModelMetadata{Family: "gemini"},
			want:      false,
		},
		{
			name:      "same family",
			source:    &discovery.ModelMetadata{Family: "claude"},
			candidate: &discovery.ModelMetadata{Family: "claude"},
			want:      true,
		},
		{
			name:      "same family with whitespace",
			source:    &discovery.ModelMetadata{Family: "  claude  "},
			candidate: &discovery.ModelMetadata{Family: "claude"},
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sameFamily(tt.source, tt.candidate)
			if got != tt.want {
				t.Errorf("sameFamily() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAbsInt(t *testing.T) {
	tests := []struct {
		name string
		x    int
		want int
	}{
		{"positive", 5, 5},
		{"negative", -5, 5},
		{"zero", 0, 0},
		{"large positive", 1000000, 1000000},
		{"large negative", -1000000, 1000000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := absInt(tt.x)
			if got != tt.want {
				t.Errorf("absInt() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEloDiff(t *testing.T) {
	tests := []struct {
		name        string
		sourceElo   *float64
		candidateElo *float64
		want        float64
	}{
		{
			name:        "both nil",
			sourceElo:   nil,
			candidateElo: nil,
			want:        0,
		},
		{
			name:        "source nil",
			sourceElo:   nil,
			candidateElo: float64Ptr(1000.0),
			want:        0,
		},
		{
			name:        "candidate nil",
			sourceElo:   float64Ptr(1000.0),
			candidateElo: nil,
			want:        0,
		},
		{
			name:        "same elo",
			sourceElo:   float64Ptr(1000.0),
			candidateElo: float64Ptr(1000.0),
			want:        0,
		},
		{
			name:        "positive diff",
			sourceElo:   float64Ptr(1000.0),
			candidateElo: float64Ptr(1200.0),
			want:        200,
		},
		{
			name:        "negative diff",
			sourceElo:   float64Ptr(1200.0),
			candidateElo: float64Ptr(1000.0),
			want:        200,
		},
		{
			name:        "large diff",
			sourceElo:   float64Ptr(1000.0),
			candidateElo: float64Ptr(2000.0),
			want:        1000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eloDiff(tt.sourceElo, tt.candidateElo)
			if got != tt.want {
				t.Errorf("eloDiff() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRankDiff(t *testing.T) {
	tests := []struct {
		name        string
		sourceRank  *int
		candidateRank *int
		want        int
	}{
		{
			name:         "both nil",
			sourceRank:   nil,
			candidateRank: nil,
			want:         0,
		},
		{
			name:         "source nil",
			sourceRank:   nil,
			candidateRank: intPtr(5),
			want:         0,
		},
		{
			name:         "candidate nil",
			sourceRank:   intPtr(5),
			candidateRank: nil,
			want:         0,
		},
		{
			name:         "same rank",
			sourceRank:   intPtr(5),
			candidateRank: intPtr(5),
			want:         0,
		},
		{
			name:         "positive diff",
			sourceRank:   intPtr(5),
			candidateRank: intPtr(7),
			want:         2,
		},
		{
			name:         "negative diff",
			sourceRank:   intPtr(7),
			candidateRank: intPtr(5),
			want:         2,
		},
		{
			name:         "large diff",
			sourceRank:   intPtr(1),
			candidateRank: intPtr(100),
			want:         99,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rankDiff(tt.sourceRank, tt.candidateRank)
			if got != tt.want {
				t.Errorf("rankDiff() = %v, want %v", got, tt.want)
			}
		})
	}
}

func float64Ptr(v float64) *float64 {
	return &v
}

func intPtr(v int) *int {
	return &v
}

func TestCapabilityOverlapEdgeCases(t *testing.T) {
	t.Run("all false", func(t *testing.T) {
		source := &discovery.ModelMetadata{
			SupportsVision:         false,
			SupportsToolCalls:      false,
			SupportsReasoning:      false,
			SupportsContentArrays:  false,
			SupportsStreamOptions:  false,
			SupportsAutoToolChoice: false,
		}
		candidate := &discovery.ModelMetadata{
			SupportsVision:         false,
			SupportsToolCalls:      false,
			SupportsReasoning:      false,
			SupportsContentArrays:  false,
			SupportsStreamOptions:  false,
			SupportsAutoToolChoice: false,
		}
		got := capabilityOverlap(source, candidate)
		if got != 0 {
			t.Errorf("expected 0 overlap, got %d", got)
		}
	})

	t.Run("all true", func(t *testing.T) {
		source := &discovery.ModelMetadata{
			SupportsVision:         true,
			SupportsToolCalls:      true,
			SupportsReasoning:      true,
			SupportsContentArrays:  true,
			SupportsStreamOptions:  true,
			SupportsAutoToolChoice: true,
		}
		candidate := &discovery.ModelMetadata{
			SupportsVision:         true,
			SupportsToolCalls:      true,
			SupportsReasoning:      true,
			SupportsContentArrays:  true,
			SupportsStreamOptions:  true,
			SupportsAutoToolChoice: true,
		}
		got := capabilityOverlap(source, candidate)
		if got != 6 {
			t.Errorf("expected 6 overlap, got %d", got)
		}
	})
}

func TestSameFamilyEdgeCases(t *testing.T) {
	t.Run("whitespace only", func(t *testing.T) {
		source := &discovery.ModelMetadata{Family: "   "}
		candidate := &discovery.ModelMetadata{Family: "   "}
		got := sameFamily(source, candidate)
		if got {
			t.Error("expected false for whitespace-only family")
		}
	})

	t.Run("case sensitive", func(t *testing.T) {
		source := &discovery.ModelMetadata{Family: "Claude"}
		candidate := &discovery.ModelMetadata{Family: "claude"}
		got := sameFamily(source, candidate)
		if got {
			t.Error("expected false for case-sensitive family comparison")
		}
	})
}

func TestEloDiffPrecision(t *testing.T) {
	t.Run("floating point precision", func(t *testing.T) {
		sourceElo := float64Ptr(1000.123456789)
		candidateElo := float64Ptr(1000.123456788)
		got := eloDiff(sourceElo, candidateElo)
		expected := math.Abs(*candidateElo - *sourceElo)
		if math.Abs(got-expected) > 1e-9 {
			t.Errorf("expected %v, got %v", expected, got)
		}
	})
}
