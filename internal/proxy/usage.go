package proxy

import (
	"context"
	"time"

	"github.com/nenya/internal/billing"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
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
	cacheMissTokens := 0
	if raw, ok := usage["prompt_cache_miss_tokens"].(float64); ok {
		cacheMissTokens = int(raw)
	}

	recordNonStreamingStats(gw, target.Model, outputTokens, cacheHitTokens, cacheMissTokens)
	recordNonStreamingMetrics(gw, target, agentName, outputTokens)
	recordCostAndBilling(ctx, gw, target, inputTokens, outputTokens)
}

func recordCostAndBilling(ctx context.Context, gw *gateway.NenyaGateway, target routing.UpstreamTarget, inputTokens, outputTokens int) {
	if gw.CostTracker == nil || (inputTokens <= 0 && outputTokens <= 0) {
		return
	}
	dm, ok := gw.ModelCatalog.Lookup(target.Model)
	if !ok || dm.Pricing == nil || dm.Pricing.IsZero() {
		return
	}
	cost := dm.Pricing.CalculateCost(int64(inputTokens), int64(outputTokens))
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

func recordNonStreamingStats(gw *gateway.NenyaGateway, model string, outputTokens, cacheHitTokens, cacheMissTokens int) {
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
}

func recordNonStreamingMetrics(gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string, outputTokens int) {
	if gw.Metrics == nil {
		return
	}
	gw.Metrics.RecordTokens("output", target.Model, agentName, target.Provider, outputTokens)
}
