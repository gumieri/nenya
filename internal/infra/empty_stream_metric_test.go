package infra

import (
	"strings"
	"testing"
)

func TestMetrics_RecordEmptyStream(t *testing.T) {
	m := NewMetrics()
	m.RecordEmptyStream("gpt-4", "openai")
	m.RecordEmptyStream("gpt-4", "openai")
	m.RecordEmptyStream("claude-3", "anthropic")

	var out strings.Builder
	m.WritePrometheus(&out)

	output := out.String()
	if !strings.Contains(output, `nenya_empty_stream_total`) {
		t.Fatalf("expected nenya_empty_stream_total metric, got:\n%s", output)
	}
	if !strings.Contains(output, `model="gpt-4"`) {
		t.Fatalf("expected gpt-4 model label, got:\n%s", output)
	}
	if !strings.Contains(output, `provider="openai"`) {
		t.Fatalf("expected openai provider label, got:\n%s", output)
	}
	if !strings.Contains(output, `model="claude-3"`) {
		t.Fatalf("expected claude-3 model label, got:\n%s", output)
	}
	if !strings.Contains(output, `provider="anthropic"`) {
		t.Fatalf("expected anthropic provider label, got:\n%s", output)
	}
	if !strings.Contains(output, "# HELP nenya_empty_stream_total") {
		t.Fatalf("expected HELP comment for nenya_empty_stream_total, got:\n%s", output)
	}
	if !strings.Contains(output, "# TYPE nenya_empty_stream_total counter") {
		t.Fatalf("expected TYPE comment for nenya_empty_stream_total, got:\n%s", output)
	}
}

func TestMetrics_RecordEmptyStream_NilMetrics(t *testing.T) {
	var m *Metrics
	m.RecordEmptyStream("gpt-4", "openai")
}

func TestMetrics_RecordEmptyStream_WritePrometheusOrder(t *testing.T) {
	m := NewMetrics()
	m.RecordEmptyStream("zeta-model", "zeta")
	m.RecordEmptyStream("alpha-model", "alpha")
	m.RecordEmptyStream("beta-model", "beta")

	var out strings.Builder
	m.WritePrometheus(&out)
	output := out.String()

	alphaIdx := strings.Index(output, `model="alpha-model"`)
	betaIdx := strings.Index(output, `model="beta-model"`)
	zetaIdx := strings.Index(output, `model="zeta-model"`)

	if alphaIdx < 0 || betaIdx < 0 || zetaIdx < 0 {
		t.Fatalf("missing entries in output")
	}
	if !(alphaIdx < betaIdx && betaIdx < zetaIdx) {
		t.Fatalf("expected alphabetical order: alpha < beta < zeta, got indices alpha=%d, beta=%d, zeta=%d", alphaIdx, betaIdx, zetaIdx)
	}
}
