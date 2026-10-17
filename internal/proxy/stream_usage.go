package proxy

import (
	"context"
	"math"
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
	acc := &streamCostAccumulator{}
	// Silent-substitution detection (NENYA-173): usage Data carries the
	// served model (the reader remembers the last one any chunk reported);
	// a mismatch is warned and counted once per dispatch, and the served
	// model's rate card prices the stream when one exists. The attribution
	// is re-resolved whenever the served model changes, so a model that
	// only appears on a later chunk still prices the cost correctly.
	var servedModel string
	substitutionReported := false
	attr := &streamAttribution{}
	// reportOnce surfaces the first mismatch exactly once per dispatch, on
	// OBSERVATION (a dispatch with no CostTracker or no usage chunk must
	// still report).
	reportOnce := func() {
		if !substitutionReported && servedModel != "" && servedModel != target.Model {
			substitutionReported = true
			reportModelSubstitution(gw, target, servedModel)
		}
	}
	// spendClaimed makes the billing request count one per dispatch: the first
	// non-zero cost delta claims the request, later usage chunks only add spend
	// (NENYA-193).
	spendClaimed := false
	return func(u stream.UsageData) {
		if u.Model != "" {
			servedModel = u.Model
			reportOnce()
		}
		completion, prompt := u.CompletionTokens, u.PromptTokens
		cacheHit, cacheMiss := u.CacheHitTokens, u.CacheMissTokens
		cacheCreation := u.CacheCreationTokens
		// UsageData.PromptTokens is already the per-event delta (the
		// stream reader clamps against its own last-value tracker), so it
		// feeds the calibration tracker directly (NENYA-135); deltas sum
		// to the request's real prompt tokens, matching the aggregate-sum
		// pairing of the tracker.
		if u.AmbiguousCacheSemantics && gw.Logger != nil {
			// The provider reported an input total with the Anthropic-native
			// cache field names and no cache decomposition, so the cache
			// dimensions were folded into the total (NENYA-167). Debug level:
			// this fires per chunk.
			gw.Logger.Debug("ambiguous usage cache semantics: folding the native cache dimensions into the input total",
				"provider", target.Provider, "model", target.Model)
		}
		gw.Calibration.RecordActual(target.Model, prompt)
		recordStreamUsageDimensions(gw, target, agentName, u)
		// Any reported dimension counts as usage for the claim (a chunk can
		// carry only cache dimensions or only a total); the accumulators are
		// advanced independently so a cache-only delta is still priced.
		usageSeen := prompt > 0 || completion > 0 || cacheHit > 0 || cacheCreation > 0 || cacheMiss > 0
		if usageSeen {
			acc.Input += int64(prompt)
			acc.Cached += int64(cacheHit)
			acc.CacheWrite += int64(cacheCreation)
			acc.Completion += int64(completion)
		}
		cost := 0.0
		if usageSeen && gw.CostTracker != nil {
			if a := attr.resolve(gw, target, servedModel); a.pricing != nil {
				cost = recordStreamCost(gw, target, a.model, a.pricing, acc)
			}
		}
		// The billing claim happens on usage presence, not on a priced cost:
		// an unpriced model's dispatch is still one billable upstream request
		// (NENYA-193).
		recordStreamSpend(ctx, gw, target, cost, acc.Input, acc.Completion, &spendClaimed, usageSeen)
	}
}

// recordStreamUsageDimensions records the Stats counters and Prometheus series
// for one usage delta. Note: the Stats cache methods don't track agent/provider,
// the Metrics methods do — the (if count > 0 { Stats.*; Metrics.* }) pattern is
// intentional.
func recordStreamUsageDimensions(gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string, u stream.UsageData) {
	if u.CompletionTokens > 0 {
		gw.Stats.RecordOutput(target.Model, u.CompletionTokens)
		gw.Metrics.RecordTokens("output", target.Model, agentName, target.Provider, u.CompletionTokens)
	}
	if u.ReasoningTokens > 0 {
		gw.Stats.RecordReasoning(target.Model, u.ReasoningTokens)
		gw.Metrics.RecordTokens(TokenDirectionReasoning, target.Model, agentName, target.Provider, u.ReasoningTokens)
	}
	if u.CacheHitTokens > 0 {
		gw.Stats.RecordCacheHit(target.Model, u.CacheHitTokens)
		gw.Metrics.RecordCacheReadTokens(target.Model, agentName, target.Provider, u.CacheHitTokens)
	}
	if u.CacheMissTokens > 0 {
		gw.Stats.RecordCacheMiss(target.Model, u.CacheMissTokens)
		gw.Metrics.RecordCacheMissTokens(target.Model, agentName, target.Provider, u.CacheMissTokens)
	}
	if u.CacheCreationTokens > 0 {
		gw.Stats.RecordCacheCreation(target.Model, u.CacheCreationTokens)
		gw.Metrics.RecordCacheCreationTokens(target.Model, agentName, target.Provider, u.CacheCreationTokens)
	}
}

// recordStreamCost prices the per-request cumulative usage and records only
// the delta since the last chunk for the attributed model, so chunk
// boundaries cannot misattribute the cached/uncached split. The pricing window is sampled at the recording
// instant (docs/COST_MODEL.md, billing approximation). It returns the cost
// delta for the billing side, which claims one request per dispatch (NENYA-193).
// streamAttribution tracks the resolved cost attribution for a streaming
// dispatch, re-resolving whenever the served model changes (NENYA-173): a
// model revealed on a later chunk must re-price the remaining usage.
type streamAttribution struct {
	resolved bool
	served   string
	attr     costAttribution
}

// resolve returns the attribution for the current served model, resolving
// it only when the model changed since the last call.
func (a *streamAttribution) resolve(gw *gateway.NenyaGateway, target routing.UpstreamTarget, servedModel string) costAttribution {
	if !a.resolved || a.served != servedModel {
		model, pricing := attributeModel(gw, target, servedModel)
		a.attr = costAttribution{model: model, pricing: pricing}
		a.served = servedModel
		a.resolved = true
	}
	return a.attr
}

// streamCostAccumulator carries the per-dispatch cumulative usage so only
// deltas are billed, plus the last-recorded cumulative cost and cached
// input PER ATTRIBUTED MODEL: a mid-stream model switch re-prices the
// cumulative under the new card, and booking that against the old model's
// recorded total would produce a negative delta (a silent budget credit
// whenever the served model is cheaper).
type streamCostAccumulator struct {
	Input      int64
	Cached     int64
	CacheWrite int64
	Completion int64
	LastCost   map[string]float64
	LastCached map[string]int64
}

// recordStreamCost prices the per-request cumulative usage under costModel
// and records only the delta since that MODEL's last recorded cumulative
// (NENYA-173: a mid-stream attribution switch books the served model's
// full cumulative, never a negative delta against the previous model).
func recordStreamCost(gw *gateway.NenyaGateway, target routing.UpstreamTarget, costModel string,
	pricing *discovery.PricingEntry, acc *streamCostAccumulator) float64 {
	if acc.LastCost == nil {
		acc.LastCost = make(map[string]float64)
	}
	if acc.LastCached == nil {
		acc.LastCached = make(map[string]int64)
	}
	now := time.Now()
	u := pricingUsage(gw, target, int(acc.Input), int(acc.Cached), int(acc.CacheWrite), int(acc.Completion), now)
	total := pricing.CalculateCost(u)
	cost := total - acc.LastCost[costModel]
	acc.LastCost[costModel] = total
	if cost == 0 {
		return 0
	}
	label := windowLabel(u.Peak)
	gw.Metrics.RecordCostWindow(costModel, label, cost)
	if cachedDelta := acc.Cached - acc.LastCached[costModel]; cachedDelta > 0 {
		gw.Metrics.RecordCachedInputTokens(costModel, label, int(cachedDelta))
	}
	acc.LastCached[costModel] = acc.Cached
	gw.CostTracker.RecordUsage(costModel, cost)
	return cost
}

// recordStreamSpend claims one billing request per dispatch: the first chunk
// that reports usage is recorded with the full entry (which increments the
// request count, whether or not pricing is configured for the model), and every
// later cost delta for the same dispatch only adds spend. Without the claim, a
// provider reporting usage in several chunks would count one request per chunk
// and emit one spend event per chunk for a single dispatch (NENYA-193).
//
// A negative cost delta (a peak-to-off-peak window flip mid-stream lowers the
// recomputed cumulative total) is recorded exactly by the cost tracker, while
// the cents-based billing side ignores it: SafeUSDCents clamps negatives to
// zero, so the billing total can only round up, never down.
func recordStreamSpend(ctx context.Context, gw *gateway.NenyaGateway, target routing.UpstreamTarget,
	cost float64, accInput, accCompletion int64, claimed *bool, usageSeen bool) {
	if gw.BillingTracker == nil {
		return
	}
	if *claimed {
		if cost > 0 {
			gw.BillingTracker.AddSpend(ctx, target.Provider, target.AccountName, cost, clampInt64ToInt(accInput), clampInt64ToInt(accCompletion))
		}
		return
	}
	if !usageSeen {
		return
	}
	*claimed = true
	claimCost := cost
	if claimCost < 0 {
		claimCost = 0
	}
	gw.BillingTracker.RecordSpend(ctx, billing.SpendEntry{
		ProviderName: target.Provider,
		AccountName:  target.AccountName,
		RequestID:    "",
		InputTokens:  clampInt64ToInt(accInput),
		OutputTokens: clampInt64ToInt(accCompletion),
		CostUSD:      claimCost,
		Timestamp:    time.Now(),
	})
}

// clampInt64ToInt narrows an int64 counter to int without the CWE-190 overflow
// wrap (AGENTS.md §7).
func clampInt64ToInt(v int64) int {
	if v > math.MaxInt {
		return math.MaxInt
	}
	if v < 0 {
		return 0
	}
	return int(v)
}
