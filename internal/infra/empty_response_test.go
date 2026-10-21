package infra

import (
	"strings"
	"testing"
)

// The empty-200 counter is the always-on degradation signal (NENYA-160):
// provider+model labeled, kind-closed, cardinality-guarded.
func TestRecordEmptyResponse(t *testing.T) {
	m := NewMetrics()
	m.RecordEmptyResponse("gpt-4o", "openai", "stream")
	m.RecordEmptyResponse("gpt-4o", "openai", "stream")
	m.RecordEmptyResponse("gpt-4o", "openai", "buffered")
	m.RecordEmptyResponse("bad name", "openai", "stream") // hostile model collapses
	m.RecordEmptyResponse("gpt-4o", "openai", "weird")    // unknown kind collapses

	var sb strings.Builder
	m.WritePrometheus(&sb)
	for _, want := range []string{
		`nenya_empty_responses_total{kind="stream", model="gpt-4o", provider="openai"} 2`,
		`nenya_empty_responses_total{kind="buffered", model="gpt-4o", provider="openai"} 1`,
		`nenya_empty_responses_total{kind="stream", model="_invalid_", provider="openai"} 1`,
		`nenya_empty_responses_total{kind="other", model="gpt-4o", provider="openai"} 1`,
	} {
		if !strings.Contains(sb.String(), want) {
			t.Fatalf("missing series %q:\n%s", want, sb.String())
		}
	}
}
