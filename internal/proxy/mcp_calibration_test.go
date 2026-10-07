package proxy

import (
	"strings"
	"testing"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"
)

// TestMCPCalibrationModel verifies the failover-aware keying (NENYA-135):
// a response model matching any chain target wins (that is the key the
// serving estimate was recorded under); a mismatched aggregator name falls
// back to the primary target; a name-less chain falls back to the response.
func TestMCPCalibrationModel(t *testing.T) {
	targets := []routing.UpstreamTarget{
		{Model: "primary-model"},
		{Model: "fallback-model"},
	}
	cases := []struct {
		name     string
		buf      *bufferedSSE
		targets  []routing.UpstreamTarget
		expected string
	}{
		{
			name:     "response matches primary target",
			buf:      &bufferedSSE{model: "primary-model"},
			targets:  targets,
			expected: "primary-model",
		},
		{
			name:     "response matches fallback target",
			buf:      &bufferedSSE{model: "fallback-model"},
			targets:  targets,
			expected: "fallback-model",
		},
		{
			name:     "aggregator-normalized name falls back to primary",
			buf:      &bufferedSSE{model: "primary-model-2026-10-07"},
			targets:  targets,
			expected: "primary-model",
		},
		{
			name:     "no targets falls back to response model",
			buf:      &bufferedSSE{model: "whatever"},
			targets:  nil,
			expected: "whatever",
		},
		{
			name:     "nil buf uses primary",
			buf:      nil,
			targets:  targets,
			expected: "primary-model",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mcpCalibrationModel(tc.targets, tc.buf); got != tc.expected {
				t.Fatalf("got %q, want %q", got, tc.expected)
			}
		})
	}
}

// TestRecordCalibrationFromBuffer verifies the per-iteration actual-side
// wiring against a buffered SSE fixture (NENYA-135).
func TestRecordCalibrationFromBuffer(t *testing.T) {
	gw := &gateway.NenyaGateway{
		Calibration: infra.NewCalibrationTracker(infra.CalibrationParams{Enabled: true, MinObservations: 1, Decay: 1 - 1e-9, ClampMin: 0.5, ClampMax: 4.0}),
	}
	buf := &bufferedSSE{rawBytes: []byte(strings.Join([]string{
		"data: {\"choices\":[{}]}",
		"",
		"data: {\"model\":\"m\",\"usage\":{\"prompt_tokens\":123,\"completion_tokens\":4}}",
		"",
		"data: [DONE]",
		"",
	}, "\n"))}

	recordCalibrationFromBuffer(gw, "m", buf)
	snap := gw.Calibration.Snapshot()["m"]
	if snap.ActualTokens != 123 {
		t.Fatalf("actual tokens = %v, want 123", snap.ActualTokens)
	}

	t.Run("nil buf and nil gw are no-ops", func(t *testing.T) {
		recordCalibrationFromBuffer(gw, "m", nil)
		recordCalibrationFromBuffer(nil, "m", buf)
	})

	t.Run("buffer without usage is a no-op", func(t *testing.T) {
		before := gw.Calibration.Snapshot()["m"].ActualTokens
		recordCalibrationFromBuffer(gw, "m", &bufferedSSE{rawBytes: []byte("data: {\"choices\":[{}]}\n\n")})
		if after := gw.Calibration.Snapshot()["m"].ActualTokens; after != before {
			t.Fatalf("usage-less buffer must not record: %v -> %v", before, after)
		}
	})
}
