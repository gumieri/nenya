package routing

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
)

// counterValue reads a counter line's value from the exposition.
func counterValue(t *testing.T, m *infra.Metrics, name, label string) int {
	t.Helper()
	var buf strings.Builder
	m.WritePrometheus(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, name) && strings.Contains(line, label) {
			idx := strings.LastIndex(line, " ")
			if idx < 0 {
				t.Fatalf("unparsable counter line %q", line)
			}
			return int(lineValue(line[idx+1:]))
		}
	}
	return 0
}

func lineValue(s string) float64 {
	var v float64
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		v = v*10 + float64(c-'0')
	}
	if neg {
		return -v
	}
	return v
}

// capTelemetryDeps builds deps with a param_compat rule (so the duplicate
// resolution and coercion run) and the given telemetry gate.
func capTelemetryDeps(m *infra.Metrics, telemetry *RequestTelemetry) TransformDeps {
	cfg := &config.Config{}
	cfg.Governance.ParamCompat = []config.ParamCompatRule{{
		ModelPrefix:    "m",
		TokenField:     "max_completion_tokens",
		IntTypedParams: []string{"max_completion_tokens"},
	}}
	return TransformDeps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config:    cfg,
		Metrics:   m,
		Telemetry: telemetry,
	}
}

// TestCapNormalizeTelemetryOncePerRequest pins AUD-35 (NENYA-248): the
// duplicate-resolution and coercion recordings fire once per LOGICAL
// request. The payload is RESTORED to the client's original shape between
// failover attempts, so every attempt re-detects the same client-shape
// findings — pre-fix the duplicate counter and coercion counter inflated by
// the attempt count.
func TestCapNormalizeTelemetryOncePerRequest(t *testing.T) {
	m := infra.NewMetrics()
	telemetry := &RequestTelemetry{}
	deps := capTelemetryDeps(m, telemetry)

	for i := 0; i < 3; i++ {
		// The client sent BOTH spellings with conflicting values (the
		// resolution keeps the smaller) under the canonical spelling —
		// every attempt re-detects the pair from the restored payload.
		payload := map[string]interface{}{
			"model":                 "m",
			"messages":              []interface{}{},
			"max_tokens":            float64(100),
			"max_completion_tokens": float64(50),
		}
		_, _, _ = TransformRequestForUpstream(deps, TransformParams{ProviderName: "prov", Model: "m", MaxOutput: 0, MaxContext: 0, Format: "openai", ReasoningEffort: ""}, payload)
	}

	if got := counterValue(t, m, "nenya_duplicate_params_total", ""); got != 1 {
		t.Fatalf("duplicate-param counter = %d after 3 attempts sharing one request, want 1 (per-attempt re-fire)", got)
	}
}

// TestCapNormalizeTelemetryNilKeepsPerAttempt pins the nil escape hatch:
// callers without request scope keep per-attempt semantics.
func TestCapNormalizeTelemetryNilKeepsPerAttempt(t *testing.T) {
	m := infra.NewMetrics()
	deps := capTelemetryDeps(m, nil)

	for i := 0; i < 3; i++ {
		payload := map[string]interface{}{
			"model":                 "m",
			"messages":              []interface{}{},
			"max_tokens":            float64(100),
			"max_completion_tokens": float64(50),
		}
		_, _, _ = TransformRequestForUpstream(deps, TransformParams{ProviderName: "prov", Model: "m", MaxOutput: 0, MaxContext: 0, Format: "openai", ReasoningEffort: ""}, payload)
	}

	if got := counterValue(t, m, "nenya_duplicate_params_total", ""); got != 3 {
		t.Fatalf("nil telemetry must keep per-attempt semantics: counter = %d, want 3", got)
	}
}
