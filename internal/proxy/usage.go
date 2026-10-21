package proxy

import (
	"context"
	"time"

	"github.com/nenya/internal/billing"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
	"github.com/nenya/internal/tracing"
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

// recordChatUsageTotals records the Stats side of a buffered response from its
// normalized usage. The MCP loop records its calibration actuals per iteration
// (recordCalibrationFromBuffer), so this stays Stats-only.
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
// object. servedModel is the model the response reported ("" when unknown):
// a different value reports the substitution and attributes cost to the
// served model's rate card when one exists (NENYA-173).
//
// The object is normalized by stream.NormalizeUsageTotals, the same helper
// the streaming reader uses, so both paths agree on what each dimension
// means (NENYA-167): prompt_tokens/input_tokens is the TOTAL input including
// the cache dimensions, and reasoning tokens are a subset of the output total.
func recordNonStreamingUsage(ctx context.Context, gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName, servedModel string, usage map[string]interface{}) {
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

	// GenAI-semconv usage attributes on the request trace (NENYA-182/187):
	// one normalized attribute set, provider spelling folded away.
	tracing.RecordUsage(ctx, tracing.UsageAttributes{
		RequestModel:  target.Model,
		ResponseModel: servedModel,
		Provider:      target.Provider,
		InputTokens:   inputTokens,
		OutputTokens:  outputTokens,
		CachedTokens:  cacheHitTokens,
		CacheCreation: cacheCreationTokens,
		Reasoning:     reasoningTokens,
	})

	// prompt_tokens is the calibration loop's ground truth (NENYA-135).
	gw.Calibration.RecordActual(target.Model, inputTokens)
	// NENYA-190: observe the pair's cache warmth (input is the TOTAL,
	// NENYA-167). A BYOK dispatch is served from the caller's own upstream
	// account (NENYA-195 stand-down): its cache hits must not pollute the
	// shared-account warmth signal the reorder tie-break consumes.
	if gw.CacheState != nil && !target.BYOK {
		gw.CacheState.Record(target.Model, target.Provider, cacheHitTokens, inputTokens)
	}
	recordNonStreamingStats(gw, target.Model, outputTokens, cacheHitTokens, cacheMissTokens, cacheCreationTokens, reasoningTokens)
	recordNonStreamingMetrics(gw, target, agentName, outputTokens, cacheHitTokens, cacheMissTokens, cacheCreationTokens, reasoningTokens)
	recordCostAndBilling(ctx, gw, target, servedModel, inputTokens, cacheHitTokens, cacheCreationTokens, outputTokens)
}

// pricingUsage builds the cost-model usage for a completed request. The
// normalized usage follows the OpenAI-style invariant (prompt tokens INCLUDE
// cached reads; the cache-hit counter is the cached subset — the Anthropic
// streaming transformer normalizes to it, and recordNonStreamingUsage maps
// native Anthropic cache reads into it), and the peak flag comes from the
// provider's peak window at the recording instant.
func pricingUsage(gw *gateway.NenyaGateway, target routing.UpstreamTarget, inputTokens, cachedInputTokens, cacheWriteTokens, outputTokens int, at time.Time) discovery.PricingUsage {
	u := discovery.PricingUsage{
		Input:           int64(inputTokens),
		CachedInput:     int64(cachedInputTokens),
		CacheWriteInput: int64(cacheWriteTokens),
		Output:          int64(outputTokens),
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

// resolveCostAttribution decides which model a dispatch's cost is
// attributed to and reports silent substitution (NENYA-173): when the
// upstream response reports a model different from the routed one, the
// substitution is warned once and counted, and the SERVED model's rate
// card is used when one exists (honest billing); otherwise the requested
// model prices it and the log line carries the flag. Returns the
// attribution model and its pricing entry (nil when unpriced).
// costAttribution is the resolved attribution for a dispatch: the model
// whose rate card prices it, and that card.
type costAttribution struct {
	model   string
	pricing *discovery.PricingEntry
}

// reportModelSubstitution warns and counts a silent substitution exactly
// once per call site (NENYA-173).
func reportModelSubstitution(gw *gateway.NenyaGateway, target routing.UpstreamTarget, servedModel string) {
	if gw == nil || target.Model == "" || servedModel == "" || servedModel == target.Model {
		return
	}
	// Bound the metric label: the served string is upstream-controlled and
	// free-form, so only catalog-known models become series; anything else
	// folds into "unknown" (the warning keeps the raw value).
	servedLabel := servedModel
	if !knownModel(gw, servedModel) {
		servedLabel = "unknown"
	}
	gw.Metrics.RecordModelSubstitution(target.Model, servedLabel)
	if gw.Logger != nil {
		gw.Logger.Warn("upstream served a different model than requested (silent substitution)",
			"requested", target.Model, "served", servedModel, "provider", target.Provider)
	}
}

// attributeModel picks the model a dispatch's cost is attributed to: the
// SERVED model when its rate card exists (honest billing), else the
// requested model.
func attributeModel(gw *gateway.NenyaGateway, target routing.UpstreamTarget, servedModel string) (string, *discovery.PricingEntry) {
	if servedModel != "" && servedModel != target.Model {
		if pricing := lookupPricing(gw, servedModel); pricing != nil {
			return servedModel, pricing
		}
		if gw != nil && gw.Logger != nil {
			gw.Logger.Debug("no rate card for the served model; pricing the requested model",
				"requested", target.Model, "served", servedModel)
		}
	}
	return target.Model, lookupPricing(gw, target.Model)
}

// knownModel reports whether the catalog knows the model (rate card
// presence aside): the metric label bound is catalog membership.
func knownModel(gw *gateway.NenyaGateway, model string) bool {
	if gw == nil || gw.ModelCatalog == nil {
		return false
	}
	_, ok := gw.ModelCatalog.Lookup(model)
	return ok
}

// lookupPricing returns the standard-rate pricing entry for a model, nil
// when the catalog has none.
func lookupPricing(gw *gateway.NenyaGateway, model string) *discovery.PricingEntry {
	if gw == nil || gw.ModelCatalog == nil {
		return nil
	}
	dm, ok := gw.ModelCatalog.Lookup(model)
	if !ok || dm.Pricing == nil || !dm.Pricing.HasStandardRate() {
		return nil
	}
	return dm.Pricing
}

// recordCostAndBilling prices one buffered dispatch and claims its billing
// request. The claim happens whenever the dispatch reported usage, even for a
// model with no pricing entry (cost 0): one dispatch that reached a provider is
// one billable upstream request (NENYA-193). servedModel is the model the
// upstream response reported ("" when unknown): a different value attributes
// cost to the served model and reports the substitution (NENYA-173).
func recordCostAndBilling(ctx context.Context, gw *gateway.NenyaGateway, target routing.UpstreamTarget, servedModel string, inputTokens, cachedInputTokens, cacheWriteTokens, outputTokens int) {
	// Report the substitution even when the usage object is empty: the
	// response model is a property of the dispatch, not of its usage.
	reportModelSubstitution(gw, target, servedModel)
	if inputTokens <= 0 && outputTokens <= 0 && cachedInputTokens <= 0 && cacheWriteTokens <= 0 {
		return
	}
	cost := 0.0
	costModel := target.Model
	pricing := lookupPricing(gw, target.Model)
	if servedModel != "" && servedModel != target.Model {
		costModel, pricing = attributeModel(gw, target, servedModel)
	}
	if pricing != nil && gw.CostTracker != nil {
		u := pricingUsage(gw, target, inputTokens, cachedInputTokens, cacheWriteTokens, outputTokens, time.Now())
		cost = pricing.CalculateCost(u)
		gw.Metrics.RecordCostWindow(costModel, windowLabel(u.Peak), cost)
		if cachedInputTokens > 0 {
			gw.Metrics.RecordCachedInputTokens(costModel, windowLabel(u.Peak), cachedInputTokens)
		}
		gw.CostTracker.RecordUsage(costModel, cost)
	}
	if gw.BillingTracker != nil && !target.BYOK {
		// A caller-funded dispatch is not the gateway's spend (NENYA-195):
		// charging it to the shared account would poison that account's
		// exhaustion/quota state and surface caller traffic as its usage.
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
