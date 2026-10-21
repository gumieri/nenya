package proxy

import (
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/testutil"
)

// NENYA-190: with auto_reorder_prefer_warm_cache on, the reorder breaks
// latency near-ties in favor of the cache-warm target; with it off (the
// default) latency alone decides.
func TestReorderTargetsPreferWarmCache(t *testing.T) {
	logger := testutil.NewTestLogger()
	secrets := &config.SecretsConfig{ClientToken: "nk-warmreorder-0123456"}
	on := true
	cfg := testutil.MinimalConfig()
	cfg.Governance.AutoReorderByLatency = &on
	cfg.Governance.AutoReorderPreferWarmCache = &on

	gw := gateway.New(t.Context(), *cfg, secrets, logger)
	// warm-model: cache-warm; cold-model: same-ish latency, no cache history.
	gw.CacheState.Record("warm-model", "p", 900, 1000)

	gw.LatencyTracker.Record("warm-model", "p", 100*1e6)
	gw.LatencyTracker.Record("warm-model", "p", 100*1e6)
	gw.LatencyTracker.Record("cold-model", "p", 103*1e6)
	gw.LatencyTracker.Record("cold-model", "p", 103*1e6)

	targets := []routing.UpstreamTarget{
		{Model: "cold-model", Provider: "p"},
		{Model: "warm-model", Provider: "p"},
	}

	// Flag on: the warm target leads the near-tie (deterministic: no jitter fn).
	sorted := reorderTargetsByLatency(&chatRequest{Payload: map[string]any{}}, gw, targets)
	if sorted[0].Model != "warm-model" {
		t.Fatalf("the warm target must lead the near-tie when the flag is on: %+v", sorted)
	}

	// Flag off (default): latency alone decides. The discriminator uses
	// medians OUTSIDE the ±5% band (the proxy path jitters randomly, and an
	// in-band pair would flip by coin-flip): the cold target is much faster,
	// so it leads even though the warm one has cache history.
	cfg.Governance.AutoReorderPreferWarmCache = nil
	gw2 := gateway.New(t.Context(), *cfg, secrets, logger)
	gw2.LatencyTracker.Record("warm-model", "p", 400*1e6)
	gw2.LatencyTracker.Record("warm-model", "p", 400*1e6)
	gw2.LatencyTracker.Record("cold-model", "p", 80*1e6)
	gw2.LatencyTracker.Record("cold-model", "p", 80*1e6)
	gw2.CacheState.Record("warm-model", "p", 900, 1000)
	sorted2 := reorderTargetsByLatency(&chatRequest{Payload: map[string]any{}}, gw2, targets)
	if sorted2[0].Model != "cold-model" {
		t.Fatalf("flag off: the faster cold target must lead: %+v", sorted2)
	}

	// Flag on with the SAME out-of-band pair: latency STILL dominates — the
	// tie-break is a near-tie rule, not a warmth-first rule.
	cfg.Governance.AutoReorderPreferWarmCache = &on
	gw3 := gateway.New(t.Context(), *cfg, secrets, logger)
	gw3.LatencyTracker.Record("warm-model", "p", 400*1e6)
	gw3.LatencyTracker.Record("warm-model", "p", 400*1e6)
	gw3.LatencyTracker.Record("cold-model", "p", 80*1e6)
	gw3.LatencyTracker.Record("cold-model", "p", 80*1e6)
	gw3.CacheState.Record("warm-model", "p", 900, 1000)
	sorted3 := reorderTargetsByLatency(&chatRequest{Payload: map[string]any{}}, gw3, targets)
	if sorted3[0].Model != "cold-model" {
		t.Fatalf("flag on: outside the band latency must stay primary: %+v", sorted3)
	}

	// The /statsz snapshot carries the observed warmth.
	if snap := gw.CacheState.Snapshot(); snap["warm-model:p"].HitRatio < 0.89 {
		t.Fatalf("the cache-state snapshot must carry the hit ratio: %+v", snap)
	}
}

// The /statsz snapshot (both forms) must carry the expanded cache_state
// section — the text form regressed to a single JSON blob once already.
func TestStatszCarriesCacheState(t *testing.T) {
	logger := testutil.NewTestLogger()
	secrets := &config.SecretsConfig{ClientToken: "nk-warmstats-01234567"}
	cfg := testutil.MinimalConfig()
	gw := gateway.New(t.Context(), *cfg, secrets, logger)
	gw.CacheState.Record("warm-model", "p", 900, 1000)

	stats := (&Proxy{}).buildStatsSnapshot(gw, true)
	section, ok := stats["cache_state"].(map[string]interface{})
	if !ok {
		t.Fatalf("cache_state must be the expanded section: %+v", stats["cache_state"])
	}
	entryRaw, ok := section["warm-model:p"].(map[string]interface{})
	if !ok || entryRaw["warm"] != true || entryRaw["model"] != "warm-model" {
		t.Fatalf("the warm pair must be snapshotted expanded: %+v", section)
	}

	// The text form renders per-field dotted paths (not one JSON blob).
	var sb strings.Builder
	renderStatsText(&sb, stats)
	out := sb.String()
	for _, want := range []string{
		"cache_state.warm-model:p.hit_ratio: 0.9",
		"cache_state.warm-model:p.warm: true",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the text form must carry %q:\n%s", want, out)
		}
	}
}
