package proxy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/resilience"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/testutil"
)

// TestRecordStreamResultBYOKSlowCallStandsDown pins AUD-14 (NENYA-260): a
// BYOK streaming request whose first effective output exceeded the slow-call
// threshold must NOT charge the shared account — no breaker failure, no
// slow-call/cb-failure metrics. NENYA-195's stand-down covers every
// shared-account write for a caller-funded dispatch; pre-fix the slow-call
// branch (unlike the success branch right below it) skipped the guard and
// emitted breaker-failure metrics for a charge against a shared account that
// never happened.
func TestRecordStreamResultBYOKSlowCallStandsDown(t *testing.T) {
	cfg := testutil.MinimalConfig()
	gw := gateway.New(context.Background(), *cfg, nil, testutil.NewTestLogger())
	t.Cleanup(gw.Close)
	gw.Metrics = infra.NewMetrics()

	byokTarget := routing.UpstreamTarget{
		Provider: "prov1", Model: "m", Format: "openai",
		CoolKey: "agent:prov1:m", BYOK: true,
	}
	gw.AgentState.CB.SetThresholdLookup(func(key string) (failure, success int) { return 1, 1 })

	signal := newSlowCallSignal(50 * time.Millisecond)
	signal.observe(500 * time.Millisecond) // well over the threshold

	recordStreamResult(gw, streamOutcome{
		target:           byokTarget,
		agentName:        "test-agent",
		cooldownDuration: time.Second,
		slowCall:         signal,
	})

	if state := gw.AgentState.CB.State(byokTarget.CoolKey); state != resilience.StateClosed {
		t.Fatalf("breaker for the BYOK target = %v, want Closed (no shared-account charge)", state)
	}
	var sb strings.Builder
	gw.Metrics.WritePrometheus(&sb)
	for _, forbidden := range []string{"nenya_slow_calls_total{", "nenya_cb_failures_total{"} {
		if strings.Contains(sb.String(), forbidden) {
			t.Fatalf("BYOK slow call must not emit %s:\n%s", forbidden, sb.String())
		}
	}
}

// TestRecordStreamResultSlowCallStillChargesShared pins the guard's boundary:
// a SHARED-account slow call is still charged (NENYA-153 unchanged).
func TestRecordStreamResultSlowCallStillChargesShared(t *testing.T) {
	cfg := testutil.MinimalConfig()
	gw := gateway.New(context.Background(), *cfg, nil, testutil.NewTestLogger())
	t.Cleanup(gw.Close)
	gw.Metrics = infra.NewMetrics()

	sharedTarget := routing.UpstreamTarget{
		Provider: "prov1", Model: "m", Format: "openai",
		CoolKey: "agent:prov1:m", BYOK: false,
	}
	gw.AgentState.CB.SetThresholdLookup(func(key string) (failure, success int) { return 1, 1 })

	signal := newSlowCallSignal(50 * time.Millisecond)
	signal.observe(500 * time.Millisecond)

	recordStreamResult(gw, streamOutcome{
		target:           sharedTarget,
		agentName:        "test-agent",
		cooldownDuration: time.Second,
		slowCall:         signal,
	})

	if state := gw.AgentState.CB.State(sharedTarget.CoolKey); state != resilience.StateOpen {
		t.Fatalf("breaker for the shared target = %v, want Open (NENYA-153 unchanged)", state)
	}
	var sb strings.Builder
	gw.Metrics.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), "nenya_slow_calls_total{") {
		t.Fatalf("a shared-account slow call must still emit the metric:\n%s", sb.String())
	}
}
