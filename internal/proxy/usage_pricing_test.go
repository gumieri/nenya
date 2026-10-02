package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"
)

// TestPricingUsagePeakWindow verifies the peak flag resolves from the
// provider's peak window at the recording instant (UTC), and that a missing
// provider is never peak.
func TestPricingUsagePeakWindow(t *testing.T) {
	provider := &config.Provider{
		Name:        "deepseek",
		PeakWindows: []config.PeakWindow{{Start: "01:00", End: "04:00", WeekdaysOnly: true}, {Start: "06:00", End: "10:00", WeekdaysOnly: true}},
	}
	gw := &gateway.NenyaGateway{Providers: map[string]*config.Provider{"deepseek": provider}}
	target := routing.UpstreamTarget{Provider: "deepseek", Model: "deepseek-flash"}

	peakTime := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	offPeakTime := time.Date(2026, 10, 2, 20, 0, 0, 0, time.UTC)

	if u := pricingUsage(gw, target, 1000, 400, 200, peakTime); !u.Peak {
		t.Error("08:00 UTC must resolve peak")
	}
	if u := pricingUsage(gw, target, 1000, 400, 200, offPeakTime); u.Peak {
		t.Error("20:00 UTC must be off-peak")
	}
	u := pricingUsage(gw, target, 1000, 400, 200, peakTime)
	if u.Input != 1000 || u.CachedInput != 400 || u.Output != 200 {
		t.Errorf("usage fields = %+v", u)
	}

	// Missing provider entry: never peak, usage still carried.
	missing := routing.UpstreamTarget{Provider: "unknown", Model: "m"}
	if u := pricingUsage(gw, missing, 10, 0, 5, peakTime); u.Peak {
		t.Error("missing provider must never be peak")
	}
}

// TestRecordCostAndBilling_PeakAndCached exercises the full buffered path:
// window selection (always-peak provider vs window-less provider) and the
// cached-input discount on the DeepSeek-style usage decomposition.
func TestRecordCostAndBilling_PeakAndCached(t *testing.T) {
	deepseekPricing := &discovery.PricingEntry{
		InputCostPer1M:       0.15,
		OutputCostPer1M:      0.60,
		PeakInputCostPer1M:   0.30,
		PeakOutputCostPer1M:  1.20,
		CachedInputCostPer1M: 0.05,
	}
	newGW := func(window []config.PeakWindow) *gateway.NenyaGateway {
		catalog := discovery.NewModelCatalog()
		catalog.Add(discovery.DiscoveredModel{ID: "deepseek-flash", Provider: "deepseek", Pricing: deepseekPricing})
		return &gateway.NenyaGateway{
			Providers:    map[string]*config.Provider{"deepseek": {Name: "deepseek", PeakWindows: window}},
			ModelCatalog: catalog,
			CostTracker:  infra.NewCostTracker(),
		}
	}
	target := routing.UpstreamTarget{Provider: "deepseek", Model: "deepseek-flash"}
	ctx := context.Background()

	// Peak: 1M input (no cache) at the peak input rate.
	gwPeak := newGW([]config.PeakWindow{{Start: "00:00", End: "23:59"}})
	recordCostAndBilling(ctx, gwPeak, target, 1_000_000, 0, 0)
	if got := gwPeak.CostTracker.GetCostMicroUSD("deepseek-flash"); got != 300_000 {
		t.Errorf("peak cost = %d microUSD, want 300000", got)
	}

	// Off-peak (window-less provider): same load at the standard rate — 2x cheaper.
	gwOff := newGW(nil)
	recordCostAndBilling(ctx, gwOff, target, 1_000_000, 0, 0)
	if got := gwOff.CostTracker.GetCostMicroUSD("deepseek-flash"); got != 150_000 {
		t.Errorf("off-peak cost = %d microUSD, want 150000", got)
	}

	// Cached half the input off-peak: cheaper than the same uncached load.
	gwCached := newGW(nil)
	recordCostAndBilling(ctx, gwCached, target, 1_000_000, 500_000, 1_000_000)
	if got := gwCached.CostTracker.GetCostMicroUSD("deepseek-flash"); got != 700_000 {
		t.Errorf("cached cost = %d microUSD, want 700000", got)
	}
	gwUncached := newGW(nil)
	recordCostAndBilling(ctx, gwUncached, target, 1_000_000, 0, 1_000_000)
	if got := gwUncached.CostTracker.GetCostMicroUSD("deepseek-flash"); got != 750_000 {
		t.Errorf("uncached cost = %d microUSD, want 750000", got)
	}
}

// TestRecordNonStreamingUsage_CacheReadThreading pins the Anthropic-style
// buffered mapping: native cache_read_input_tokens is added into input (the
// adapter's prompt_tokens excludes cache reads) and becomes the cached subset.
func TestRecordNonStreamingUsage_CacheReadThreading(t *testing.T) {
	catalog := discovery.NewModelCatalog()
	catalog.Add(discovery.DiscoveredModel{ID: "m", Provider: "p", Pricing: &discovery.PricingEntry{
		InputCostPer1M: 1.0, OutputCostPer1M: 0.0, CachedInputCostPer1M: 0.10,
	}})
	gw := &gateway.NenyaGateway{
		Providers:    map[string]*config.Provider{"p": {Name: "p"}},
		ModelCatalog: catalog,
		CostTracker:  infra.NewCostTracker(),
	}
	target := routing.UpstreamTarget{Provider: "p", Model: "m"}
	// Anthropic-style: input_tokens=900_000 excludes 100_000 cache reads.
	usage := map[string]interface{}{
		"prompt_tokens":           900_000.0,
		"completion_tokens":       0.0,
		"total_tokens":            900_000.0,
		"cache_read_input_tokens": 100_000.0,
	}
	recordNonStreamingUsage(context.Background(), gw, target, "agent", usage)
	// billed input = 1_000_000: 100k cached at 0.10 (0.01) + 900k at 1.0 (0.90) = 0.91
	if got := gw.CostTracker.GetCostMicroUSD("m"); got != 910_000 {
		t.Errorf("cost = %d microUSD, want 910000", got)
	}
}
