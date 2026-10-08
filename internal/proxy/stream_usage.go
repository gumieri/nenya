package proxy

import (
	"context"
	"time"

	"github.com/nenya/internal/billing"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
)

// makeUsageCallback returns a callback function that records token usage statistics.
// The callback is invoked by the SSE transformer when usage metadata is received.
// The ctx parameter is reserved for future timeout/cancellation logic in cost tracking.
//
// Usage chunks carry per-chunk DELTAS of the upstream cumulative counters, and
// the cached/uncached split of a delta need not respect cached ⊆ prompt (the
// split can lag across chunks). Cost is therefore computed on the per-request
// CUMULATIVE totals and only the cost delta is recorded, keeping the recorded
// series exact regardless of chunk boundaries.
func (p *Proxy) makeUsageCallback(ctx context.Context, gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string) func(stream.UsageData) {
	var accInput, accCached, accCompletion int64
	var lastCost float64
	var lastCached int64
	return func(u stream.UsageData) {
		completion, prompt := u.CompletionTokens, u.PromptTokens
		cacheHit, cacheMiss := u.CacheHitTokens, u.CacheMissTokens
		cacheCreation, reasoning := u.CacheCreationTokens, u.ReasoningTokens
		// UsageData.PromptTokens is already the per-event delta (the
		// stream reader clamps against its own last-value tracker), so it
		// feeds the calibration tracker directly (NENYA-135); deltas sum
		// to the request's real prompt tokens, matching the aggregate-sum
		// pairing of the tracker.
		gw.Calibration.RecordActual(target.Model, prompt)
		if completion > 0 {
			gw.Stats.RecordOutput(target.Model, completion)
			gw.Metrics.RecordTokens("output", target.Model, agentName, target.Provider, completion)
		}
		if reasoning > 0 {
			gw.Stats.RecordReasoning(target.Model, reasoning)
			gw.Metrics.RecordTokens(TokenDirectionReasoning, target.Model, agentName, target.Provider, reasoning)
		}
		if cacheHit > 0 {
			gw.Stats.RecordCacheHit(target.Model, cacheHit)
			gw.Metrics.RecordCacheReadTokens(target.Model, agentName, target.Provider, cacheHit)
		}
		if cacheMiss > 0 {
			gw.Stats.RecordCacheMiss(target.Model, cacheMiss)
			gw.Metrics.RecordCacheMissTokens(target.Model, agentName, target.Provider, cacheMiss)
		}
		if cacheCreation > 0 {
			gw.Stats.RecordCacheCreation(target.Model, cacheCreation)
			gw.Metrics.RecordCacheCreationTokens(target.Model, agentName, target.Provider, cacheCreation)
		}
		// Note: Stats cache methods don't track agent/provider; Metrics methods do.
		// This pattern (if count > 0 { Stats.*; Metrics.* }) is intentional.
		if gw.CostTracker != nil && (prompt > 0 || completion > 0) {
			if dm, ok := gw.ModelCatalog.Lookup(target.Model); ok && dm.Pricing != nil && dm.Pricing.HasStandardRate() {
				accInput += int64(prompt)
				accCached += int64(cacheHit)
				accCompletion += int64(completion)
				recordStreamCost(ctx, gw, target, dm.Pricing, accInput, accCached, accCompletion, &lastCost, &lastCached, prompt, completion)
			}
		}
	}
}

// recordStreamCost prices the per-request cumulative usage and records only
// the delta since the last chunk, so chunk boundaries cannot misattribute the
// cached/uncached split. The pricing window is sampled at the recording
// instant (docs/COST_MODEL.md, billing approximation).
func recordStreamCost(ctx context.Context, gw *gateway.NenyaGateway, target routing.UpstreamTarget,
	pricing *discovery.PricingEntry, accInput, accCached, accCompletion int64, lastCost *float64, lastCached *int64, prompt, completion int) {
	now := time.Now()
	u := pricingUsage(gw, target, int(accInput), int(accCached), int(accCompletion), now)
	total := pricing.CalculateCost(u)
	cost := total - *lastCost
	*lastCost = total
	if cost == 0 {
		return
	}
	label := windowLabel(u.Peak)
	gw.Metrics.RecordCostWindow(target.Model, label, cost)
	if cachedDelta := accCached - *lastCached; cachedDelta > 0 {
		gw.Metrics.RecordCachedInputTokens(target.Model, label, int(cachedDelta))
	}
	*lastCached = accCached
	gw.CostTracker.RecordUsage(target.Model, cost)
	if gw.BillingTracker != nil {
		gw.BillingTracker.RecordSpend(ctx, billing.SpendEntry{
			ProviderName: target.Provider,
			AccountName:  target.AccountName,
			RequestID:    "",
			InputTokens:  prompt,
			OutputTokens: completion,
			CostUSD:      cost,
			Timestamp:    now,
		})
	}
}
