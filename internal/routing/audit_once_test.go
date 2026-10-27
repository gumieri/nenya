package routing

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
)

// unknownFieldCount reads the nenya_unknown_request_fields_total counter for
// the openai format from the metrics exposition.
func unknownFieldCount(t *testing.T, m *infra.Metrics) int {
	t.Helper()
	var buf strings.Builder
	m.WritePrometheus(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "nenya_unknown_request_fields_total{") &&
			strings.Contains(line, `format="openai"`) {
			idx := strings.LastIndex(line, " ")
			if idx < 0 {
				t.Fatalf("unparsable counter line %q", line)
			}
			var v float64
			if _, err := fmt.Sscan(line[idx+1:], &v); err != nil {
				t.Fatalf("unparsable counter line %q", line)
			}
			return int(v)
		}
	}
	return 0
}

// TestAuditRequestFieldsOncePerRequest pins AUD-39 (NENYA-245): the
// unknown-fields audit fires once per LOGICAL request, not once per
// failover attempt. The retry loop owns one flag shared across the deps it
// builds per attempt; pre-fix every attempt incremented the counter and
// duplicated the Warn, so failover-heavy traffic inflated
// nenya_unknown_request_fields_total by the attempt count.
func TestAuditRequestFieldsOncePerRequest(t *testing.T) {
	m := infra.NewMetrics()
	telemetry := RequestTelemetry{}
	deps := TransformDeps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config:    &config.Config{},
		Metrics:   m,
		Telemetry: &telemetry,
	}
	payload := map[string]interface{}{
		"model":            "m",
		"messages":         []interface{}{},
		"client_telemetry": "whatever", // unknown field
	}

	for i := 0; i < 3; i++ {
		_, _, _ = TransformRequestForUpstream(deps, TransformParams{ProviderName: "prov", Model: "m", MaxOutput: 0, MaxContext: 0, Format: "openai", ReasoningEffort: ""}, payload)
	}

	if got := unknownFieldCount(t, m); got != 1 {
		t.Fatalf("unknown-request-field counter = %d after 3 attempts sharing one request, want 1 (per-attempt re-fire)", got)
	}
}

// TestAuditRequestFieldsNilFlagKeepsPerAttempt pins the escape hatch:
// callers without request scope (nil flag) keep per-attempt semantics.
func TestAuditRequestFieldsNilFlagKeepsPerAttempt(t *testing.T) {
	m := infra.NewMetrics()
	deps := TransformDeps{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config:  &config.Config{},
		Metrics: m,
	}
	payload := map[string]interface{}{
		"model":            "m",
		"messages":         []interface{}{},
		"client_telemetry": "whatever",
	}
	for i := 0; i < 3; i++ {
		_, _, _ = TransformRequestForUpstream(deps, TransformParams{ProviderName: "prov", Model: "m", MaxOutput: 0, MaxContext: 0, Format: "openai", ReasoningEffort: ""}, payload)
	}
	if got := unknownFieldCount(t, m); got != 3 {
		t.Fatalf("nil flag must keep per-attempt semantics: counter = %d, want 3", got)
	}
}
