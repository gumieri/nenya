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

// extractTokenCounts extracts input, output, and total token counts from a
// usage object in any accepted placement or wire shape (NENYA-167), so an
// int-typed or native-shaped map is read the same way as a JSON-decoded
// OpenAI-shaped one.
func extractTokenCounts(usage map[string]interface{}) (inputTokens, outputTokens, totalTokens int) {
	totals := stream.NormalizeUsageTotals(usage)
	inputTokens, outputTokens, totalTokens = totals.Prompt, totals.Completion, totals.Total
	// A provider that reports only a grand total (or total + prompt) still has
	// a derivable output count; keep the derivation the pre-NENYA-167 code
	// performed.
	if outputTokens == 0 && totalTokens > inputTokens {
		outputTokens = totalTokens - inputTokens
	}
	return
}

func recordChatUsage(gw *gateway.NenyaGateway, model string, usage map[string]interface{}) {
	_, outputTokens, totalTokens := extractTokenCounts(usage)
	if totalTokens <= 0 {
		return
	}
	// Calibration actuals for the MCP loop are recorded per iteration in
	// mcpIteration (recordCalibrationFromBuffer) — pairing K dispatch
	// estimates with K actuals; this helper stays Stats-only.
	if gw.Stats != nil {
		gw.Stats.RecordOutput(model, outputTokens)
	}
}

// recordChatUsageTotals records the Stats side of a buffered response from its
// normalized usage. The MCP loop records its calibration actuals per iteration
// (recordCalibrationFromBuffer), so this stays Stats-only, as recordChatUsage
// does.
func recordChatUsageTotals(gw *gateway.NenyaGateway, model string, totals stream.UsageTotals) {
	if totals.Total <= 0 {
		return
	}
	if gw.Stats != nil {
		gw.Stats.RecordOutput(model, totals.Completion)
	}
}

func recordUsageFromMap(gw *gateway.NenyaGateway, responseMap map[string]interface{}, model, providerName string) {
	usage, ok := stream.UsageMapFromChunk(responseMap)
	if !ok {
		return
	}
	_, outputTokens, totalTokens := extractTokenCounts(usage)
	if totalTokens <= 0 {
		return
	}
	if gw.Stats != nil {
		gw.Stats.RecordOutput(model, outputTokens)
	}
	if providerName != "" {
		gw.Metrics.RecordTokens("output", model, "", providerName, outputTokens)
	}
}

// recordNonStreamingUsage records a completed (non-streaming) upstream usage
// object. The object is normalized by stream.NormalizeUsageTotals, the same
// helper the streaming reader uses, so both paths agree on what each dimension
// means (NENYA-167): prompt_tokens/input_tokens is the TOTAL input including
// the cache dimensions, and reasoning tokens are a subset of the output total.
func recordNonStreamingUsage(ctx context.Context, gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string, usage map[string]interface{}) {
	totals := stream.NormalizeUsageTotals(usage)
	if totals.AmbiguousCacheSemantics && gw.Logger != nil {
		// The provider reported an input total together with the
		// Anthropic-native cache field names and no cache decomposition: the
		// cache dimensions were folded into the total (the Anthropic reading).
		// A provider that reports an inclusive total under those field names
		// would be over-counted here, so make the assumption visible.
		gw.Logger.Warn("ambiguous usage cache semantics: folding the native cache dimensions into the input total",
			"provider", target.Provider, "model", target.Model)
	}
	inputTokens := totals.Prompt
	outputTokens := totals.Completion
	cacheHitTokens := totals.CacheHit
	cacheMissTokens := totals.CacheMiss
	cacheCreationTokens := totals.CacheCreation
	reasoningTokens := totals.Reasoning

	// prompt_tokens is the calibration loop's ground truth (NENYA-135).
	gw.Calibration.RecordActual(target.Model, inputTokens)
	recordNonStreamingStats(gw, target.Model, outputTokens, cacheHitTokens, cacheMissTokens, cacheCreationTokens, reasoningTokens)
	recordNonStreamingMetrics(gw, target, agentName, outputTokens, cacheHitTokens, cacheMissTokens, cacheCreationTokens, reasoningTokens)
	recordCostAndBilling(ctx, gw, target, inputTokens, cacheHitTokens, outputTokens)
}

// pricingUsage builds the cost-model usage for a completed request. The
// normalized usage follows the OpenAI-style invariant (prompt tokens INCLUDE
// cached reads; the cache-hit counter is the cached subset — the Anthropic
// streaming transformer normalizes to it, and recordNonStreamingUsage maps
// native Anthropic cache reads into it), and the peak flag comes from the
// provider's peak window at the recording instant.
func pricingUsage(gw *gateway.NenyaGateway, target routing.UpstreamTarget, inputTokens, cachedInputTokens, outputTokens int, at time.Time) discovery.PricingUsage {
	u := discovery.PricingUsage{
		Input:       int64(inputTokens),
		CachedInput: int64(cachedInputTokens),
		Output:      int64(outputTokens),
	}
	u.Peak = gw.Providers[target.Provider].IsPeakAt(at)
	return u
}

// windowLabel maps a resolved window to its metric label.
func windowLabel(peak bool) string {
	if peak {
		return "peak"
	}
	return "offpeak"
}

func recordCostAndBilling(ctx context.Context, gw *gateway.NenyaGateway, target routing.UpstreamTarget, inputTokens, cachedInputTokens, outputTokens int) {
	if gw.CostTracker == nil || (inputTokens <= 0 && outputTokens <= 0) {
		return
	}
	dm, ok := gw.ModelCatalog.Lookup(target.Model)
	if !ok || dm.Pricing == nil || !dm.Pricing.HasStandardRate() {
		return
	}
	u := pricingUsage(gw, target, inputTokens, cachedInputTokens, outputTokens, time.Now())
	cost := dm.Pricing.CalculateCost(u)
	gw.Metrics.RecordCostWindow(target.Model, windowLabel(u.Peak), cost)
	if cachedInputTokens > 0 {
		gw.Metrics.RecordCachedInputTokens(target.Model, windowLabel(u.Peak), cachedInputTokens)
	}
	gw.CostTracker.RecordUsage(target.Model, cost)
	if gw.BillingTracker != nil {
		gw.BillingTracker.RecordSpend(ctx, billing.SpendEntry{
			ProviderName: target.Provider,
			AccountName:  target.AccountName,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			CostUSD:      cost,
			Timestamp:    time.Now(),
		})
	}
}

func recordNonStreamingStats(gw *gateway.NenyaGateway, model string, outputTokens, cacheHitTokens, cacheMissTokens, cacheCreationTokens, reasoningTokens int) {
	if gw.Stats == nil {
		return
	}
	gw.Stats.RecordOutput(model, outputTokens)
	if cacheHitTokens > 0 {
		gw.Stats.RecordCacheHit(model, cacheHitTokens)
	}
	if cacheMissTokens > 0 {
		gw.Stats.RecordCacheMiss(model, cacheMissTokens)
	}
	if cacheCreationTokens > 0 {
		gw.Stats.RecordCacheCreation(model, cacheCreationTokens)
	}
	if reasoningTokens > 0 {
		gw.Stats.RecordReasoning(model, reasoningTokens)
	}
}

func recordNonStreamingMetrics(gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string, outputTokens, cacheHitTokens, cacheMissTokens, cacheCreationTokens, reasoningTokens int) {
	if gw.Metrics == nil {
		return
	}
	gw.Metrics.RecordTokens("output", target.Model, agentName, target.Provider, outputTokens)
	if cacheHitTokens > 0 {
		gw.Metrics.RecordCacheReadTokens(target.Model, agentName, target.Provider, cacheHitTokens)
	}
	if cacheMissTokens > 0 {
		gw.Metrics.RecordCacheMissTokens(target.Model, agentName, target.Provider, cacheMissTokens)
	}
	if cacheCreationTokens > 0 {
		gw.Metrics.RecordCacheCreationTokens(target.Model, agentName, target.Provider, cacheCreationTokens)
	}
	if reasoningTokens > 0 {
		gw.Metrics.RecordTokens(TokenDirectionReasoning, target.Model, agentName, target.Provider, reasoningTokens)
	}
}
