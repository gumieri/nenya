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

// extractTokenCounts extracts input, output, and total token counts from a usage map.
func extractTokenCounts(usage map[string]interface{}) (inputTokens, outputTokens, totalTokens int) {
	if raw, ok := usage["total_tokens"].(float64); ok {
		totalTokens = int(raw)
	}
	if raw, ok := usage["prompt_tokens"].(float64); ok {
		inputTokens = int(raw)
	}
	if raw, ok := usage["completion_tokens"].(float64); ok {
		outputTokens = int(raw)
	}
	if totalTokens > 0 && outputTokens == 0 {
		outputTokens = totalTokens - inputTokens
	}
	return
}

func recordChatUsage(gw *gateway.NenyaGateway, model string, usage map[string]interface{}) {
	_, outputTokens, totalTokens := extractTokenCounts(usage)
	if totalTokens <= 0 {
		return
	}
	if gw.Stats != nil {
		gw.Stats.RecordOutput(model, outputTokens)
	}
}

func recordUsageFromMap(gw *gateway.NenyaGateway, responseMap map[string]interface{}, model, providerName string) {
	usage, ok := responseMap["usage"].(map[string]interface{})
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

func recordNonStreamingUsage(ctx context.Context, gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string, usage map[string]interface{}) {
	outputTokens := 0
	if raw, ok := usage["completion_tokens"].(float64); ok {
		outputTokens = int(raw)
	}
	inputTokens := 0
	if raw, ok := usage["prompt_tokens"].(float64); ok {
		inputTokens = int(raw)
	}
	cacheHitTokens := 0
	if raw, ok := usage["prompt_cache_hit_tokens"].(float64); ok {
		cacheHitTokens = int(raw)
	}
	if cacheHitTokens == 0 {
		// OpenAI-style detail fallback: some providers report cached tokens
		// only under prompt_tokens_details (the streaming reader has the
		// same fallback — keep the two paths consistent).
		if details, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
			if raw, ok := details["cached_tokens"].(float64); ok {
				cacheHitTokens = int(raw)
			}
		}
	}
	if cacheHitTokens == 0 {
		// Anthropic-style buffered responses (converted by the adapter to
		// OpenAI shape with prompt_tokens = input_tokens, which EXCLUDES
		// cache reads) report reads natively. Only apply when no
		// OpenAI-style decomposition exists, or cache reads would be
		// double-counted into input.
		if raw, ok := usage["cache_read_input_tokens"].(float64); ok && raw > 0 {
			cacheHitTokens = int(raw)
			inputTokens += cacheHitTokens
		}
	}
	cacheMissTokens := 0
	if raw, ok := usage["prompt_cache_miss_tokens"].(float64); ok {
		cacheMissTokens = int(raw)
	}
	cacheCreationTokens := 0
	if raw, ok := usage["cache_creation_tokens"].(float64); ok {
		cacheCreationTokens = int(raw)
	}
	if raw, ok := usage["cache_creation_input_tokens"].(float64); ok && cacheCreationTokens == 0 {
		cacheCreationTokens = int(raw)
	}
	// Anthropic non-streaming responses return cache_creation_input_tokens natively
	// (not normalized by the transformer), so we check it as a fallback for providers
	// that use that field name.

	// Reasoning/thinking tokens across every provider spelling (NENYA-33):
	// OpenAI output_tokens_details.reasoning_tokens, legacy
	// completion_tokens_details, Anthropic completion_reasoning_tokens,
	// Gemini thoughtsTokenCount, flat reasoning_tokens.
	reasoningTokens := stream.ExtractReasoningTokens(usage)

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
