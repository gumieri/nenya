package proxy

import (
	"strings"
	"testing"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/testutil"
)

// NENYA-160: an HTTP-200 body with no choices counts the empty-response
// degradation signal; a populated one does not.
func TestCountEmptyChoices(t *testing.T) {
	gw := &gateway.NenyaGateway{Logger: testutil.NewTestLogger(), Metrics: infra.NewMetrics()}
	target := routing.UpstreamTarget{Model: "gpt-4o", Provider: "openai", Format: "openai"}

	countEmptyChoices(gw, target, map[string]interface{}{
		"choices": []interface{}{map[string]any{"message": map[string]any{}}},
	})
	var sb strings.Builder
	gw.Metrics.WritePrometheus(&sb)
	if strings.Contains(sb.String(), "nenya_empty_responses_total{") {
		t.Fatalf("a populated response must not count:\n%s", sb.String())
	}

	countEmptyChoices(gw, target, map[string]interface{}{})
	countEmptyChoices(gw, target, map[string]interface{}{"choices": []interface{}{}})
	sb.Reset()
	gw.Metrics.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `nenya_empty_responses_total{kind="buffered", model="gpt-4o", provider="openai"} 2`) {
		t.Fatalf("empty bodies must count:\n%s", sb.String())
	}
}
