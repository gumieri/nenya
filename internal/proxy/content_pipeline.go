package proxy

import (
	"context"
	"net/http"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/pipeline"
	providerpkg "github.com/nenya/internal/providers"
	"github.com/nenya/internal/routing"
)

// cacheAwareActive reports whether the per-agent cache_aware policy should skip
// the history-wide window compaction and the TF-IDF tail prune for this request.
// It is active only when the agent opted in, the payload fits the known hard
// limit (and no window guard could fire when the limit is unknown), and (for
// "auto") the resolved provider caches prompt prefixes automatically.
//
// The provider signal is the primary target's provider name resolved against
// the built-in provider specs; "auto" therefore recognizes the built-in
// cache-rich providers (deepseek/anthropic/openai) and custom or renamed
// providers should use "force". Gating is evaluated once against the primary
// target, before dispatch; an agent that mixes cache-rich and cache-poor
// providers should use a stable strategy (or off/force) so the policy matches
// the target that actually serves.
func cacheAwareActive(agent *config.AgentConfig, provider string, tokenCount, hardLimit, windowMaxCtx int) bool {
	if agent == nil {
		return false
	}
	switch agent.CacheAware {
	case config.CacheAwareForce:
		// Force: no provider-capability check.
	case config.CacheAwareAuto:
		if !providerpkg.SupportsAutomaticPrefixCache(provider) {
			return false
		}
	default:
		return false
	}
	// A known hard limit that the payload already exceeds needs the trim
	// stages. When the limit is unknown, only skip if no model-resolved
	// window guard could fire (an unknown limit plus a known window guard
	// would remove the last size guard).
	if hardLimit > 0 {
		if tokenCount >= hardLimit {
			return false
		}
	} else if windowMaxCtx > 0 {
		return false
	}
	return true
}

// applyContentPipeline runs the shared preprocessing stages (prefix
// cache optimization, compaction, windowing, interceptor chain) over the
// request payload. Mutations are applied in place. The first return
// reports whether the entropy interceptor redacted high-entropy spans
// from this request (an egress-screen trigger).
func (p *Proxy) applyContentPipeline(gw *gateway.NenyaGateway, ctx context.Context, opts contentPipelineOpts) (bool, error) {
	payload := opts.Payload
	if gw.InterceptorChain == nil {
		return false, nil
	}

	messages, ok := payload["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		return false, nil
	}

	pipeline.ApplyPrefixCacheOptimizations(payload, messages, gw.Config.PrefixCache)

	// The gate must see the payload as it is now (after auto-search/MCP
	// injection, which ran in resolvePipelineContext after req.TokenCount was
	// captured), or a payload that actually exceeds the hard limit could be
	// treated as fitting.
	gateTokens := gw.CountRequestTokens(payload)
	cacheAware := cacheAwareActive(opts.Agent, opts.TargetProvider, gateTokens, opts.HardLimit, opts.WindowMaxCtx)

	if !opts.Profile.IsIDE {
		if pipeline.ApplyCompaction(messages, gw.Config.Compaction) {
			gw.Metrics.RecordCompaction()
		}
	}

	if !opts.Profile.IsIDE {
		if pipeline.PruneStaleToolCalls(payload, gw.Config.Compaction) {
			gw.Metrics.RecordCompaction()
		}
		if pipeline.PruneThoughts(payload, gw.Config.Compaction) {
			gw.Metrics.RecordCompaction()
		}
	}

	// cache_aware: the payload fits and the provider caches prefixes, so skip
	// the history-wide window rewrite and let the provider cache do the work.
	applyWindowStage(gw, ctx, opts, payload, messages, gateTokens, cacheAware)

	msgs, ok := payload["messages"].([]interface{})
	if !ok {
		// A stage rewrote messages into a non-array shape: treat as empty
		// rather than panicking (defensive; stages preserve the array).
		return false, nil
	}
	messages = msgs
	if len(messages) == 0 {
		return false, nil
	}

	msgObjs := make([]map[string]any, len(messages))
	for i, m := range messages {
		if obj, ok := m.(map[string]any); ok {
			msgObjs[i] = obj
		}
	}

	req := &pipeline.InterceptRequest{
		Payload:        payload,
		Messages:       msgObjs,
		AgentName:      opts.AgentName,
		Agent:          opts.Agent,
		Profile:        opts.Profile,
		SoftLimit:      opts.SoftLimit,
		HardLimit:      opts.HardLimit,
		TokenCount:     opts.TokenCount,
		SkipTFIDFPrune: cacheAware,
	}

	if _, err := gw.InterceptorChain.Execute(ctx, req); err != nil {
		return req.EntropyRedacted, err
	}
	return req.EntropyRedacted, nil
}

// applyWindowStage runs window compaction unless the cache_aware policy skipped
// the history-wide rewrite (and the tail TF-IDF prune) for this request.
// tokenCount is the current (post-injection) payload size.
func applyWindowStage(gw *gateway.NenyaGateway, ctx context.Context, opts contentPipelineOpts, payload map[string]any, messages []any, tokenCount int, cacheAware bool) {
	if cacheAware {
		return
	}
	deps := buildWindowDeps(gw)
	if windowed, err := pipeline.ApplyWindowCompaction(ctx, deps, payload, messages, tokenCount, gw.Config.Window, opts.WindowMaxCtx, gw.CountRequestTokens); err != nil {
		gw.Logger.Warn("window compaction failed, proceeding without it", "err", err)
	} else if windowed {
		gw.Metrics.RecordWindow(gw.Config.Window.Mode)
		gw.Metrics.RecordTokensSaved("window", tokenCount-gw.CountRequestTokens(payload))
	}
}

// buildWindowDeps creates a WindowDeps from the gateway state.
func buildWindowDeps(gw *gateway.NenyaGateway) pipeline.WindowDeps {
	jc := gw.Config.Governance.Judgments["summary_fidelity"]
	return pipeline.WindowDeps{
		Logger:    gw.Logger,
		ClientFor: gw.ClientFor,
		Providers: gw.Providers,
		InjectAPIKey: func(providerName string, headers http.Header) error {
			return routing.InjectAPIKeyWithGateway(providerName, gw, headers)
		},
		CountTokens:        gw.CountTokens,
		SummaryCache:       gw.WindowSummaries,
		HeadCache:          gw.WindowHeads,
		FidelityGate:       gw.WindowFidelityGate,
		FidelityGateStrict: jc != nil && jc.Action == "strict",
		Metrics:            gw.Metrics,
	}
}
