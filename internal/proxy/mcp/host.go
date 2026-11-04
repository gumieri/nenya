package mcpproxy

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
)

// Host is the seam between the MCP subsystem and its hosting proxy: every
// parent-package function the moved code consumes is a Host method. The
// parent supplies proxyHost, a thin delegating adapter. Host methods use
// only mcpproxy types, stdlib types, and types from other internal packages.
//
// Note for implementers: some adapters delegate to parent methods that
// dereference the wrapped *Proxy (PrepareAndSend, HandleUpstreamError);
// a Host carrying a nil proxy must not be used for dispatching paths.
// Some interface methods accept the host for signature uniformity with
// their call sites while never dereferencing it (GuardBufferedLine,
// CanaryBufferedLine, InjectMCPSystemPrompt, the auto-search helpers) —
// implementations must not read hidden state from that fact.
type Host interface {
	// APIKeyName renders an API key's safe logging/metric name.
	APIKeyName(apiKey *config.ApiKey) string
	// ApplyToolDescriptionSpotlight applies the untrusted-content envelope
	// to injected tool descriptions.
	ApplyToolDescriptionSpotlight(tools []map[string]any, settings pipeline.SpotlightSettings, metrics *infra.Metrics)
	// ArmClientWriteDeadline re-arms the per-write client-stall deadline.
	ArmClientWriteDeadline(w http.ResponseWriter, timeout time.Duration)
	// CalculateBackoff computes the exponential backoff with jitter.
	CalculateBackoff(attempt int) time.Duration
	// ExfilGuardFor resolves the per-agent ExfilGuard policy.
	ExfilGuardFor(gw *gateway.NenyaGateway, agentName string) *stream.ExfilGuard
	// HandleUpstreamError classifies one upstream error and returns the
	// retry signal, the parsed retry delay, and the quota flag.
	HandleUpstreamError(gw *gateway.NenyaGateway, idx int, targets []routing.UpstreamTarget, target routing.UpstreamTarget, cooldownDuration time.Duration, agentName string, action UpstreamAction) (RetrySignal, time.Duration, bool)
	// MatchRequestScopedError reports provider-configured client-fault errors.
	MatchRequestScopedError(gw *gateway.NenyaGateway, providerName string, statusCode int, body []byte) *config.RequestScopedErrorRule
	// PrepareAndSend dispatches one upstream attempt and returns its action.
	PrepareAndSend(gw *gateway.NenyaGateway, r *http.Request, dp DispatchParams) UpstreamAction
	// RecordBufferedOutcome records the breaker outcome of a served buffered call.
	RecordBufferedOutcome(gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string, dispatchAt, bodyReadAt time.Time, cooldown time.Duration)
	// RecordChatUsageTotals records the per-model usage counters.
	RecordChatUsageTotals(gw *gateway.NenyaGateway, model string, totals stream.UsageTotals)
	// RecordCostAndBilling records cost and one billing request.
	RecordCostAndBilling(ctx context.Context, gw *gateway.NenyaGateway, target routing.UpstreamTarget, servedModel string, inputTokens, cachedInputTokens, cacheWriteTokens, outputTokens int)
	// RecordNonStreamingMetrics records per-target non-streaming metrics.
	RecordNonStreamingMetrics(gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string, outputTokens, cacheHitTokens, cacheMissTokens, cacheCreationTokens, reasoningTokens int)
	// RecordNonStreamingStats records per-model non-streaming Stats counters.
	RecordNonStreamingStats(gw *gateway.NenyaGateway, model string, outputTokens, cacheHitTokens, cacheMissTokens, cacheCreationTokens, reasoningTokens int)
	// ReserveKeyBudget reserves the caller's daily key token budget.
	ReserveKeyBudget(gw *gateway.NenyaGateway, key *config.ApiKey, tokenCount int) (string, bool)
	// ResolveStreamStallTimeout resolves the configured client-stall window.
	ResolveStreamStallTimeout(gw *gateway.NenyaGateway) time.Duration
	// ResolveTransformer picks the response transformer for a target.
	ResolveTransformer(gw *gateway.NenyaGateway, target routing.UpstreamTarget, sourceFormat string, sigCache infra.ThoughtSignatureKV) stream.ResponseTransformer
	// ScanToolArgsCanary applies the canary tripwire to outbound tool arguments.
	ScanToolArgsCanary(canary pipeline.CanarResult, args map[string]any, metrics *infra.Metrics, logger *slog.Logger, tool string) (tripped, refuse bool)
	// ScopedThoughtSigCache returns the caller-namespaced signature view.
	ScopedThoughtSigCache(gw *gateway.NenyaGateway, apiKey *config.ApiKey) infra.ThoughtSignatureKV
	// SpotlightSettingsFor resolves the spotlight settings for an agent.
	SpotlightSettingsFor(cfg *config.Config, agent config.AgentConfig) pipeline.SpotlightSettings
	// WaitWithCancel waits d or until ctx is canceled.
	WaitWithCancel(ctx context.Context, d time.Duration)
	// WriteExfilBlockedSSE writes the structured exfil-blocked payload.
	WriteExfilBlockedSSE(gw *gateway.NenyaGateway, w http.ResponseWriter)
	// WriteExfilDetectedSSE writes the structured canary-detected payload.
	WriteExfilDetectedSSE(gw *gateway.NenyaGateway, w http.ResponseWriter)
	// WriteGatewayStreamError writes an SSE error event with the given type.
	WriteGatewayStreamError(w http.ResponseWriter, statusCode int, errType infra.ErrorType, message string)
	// WritePipelineRejection renders a pipeline rejection to the client.
	WritePipelineRejection(gw *gateway.NenyaGateway, w http.ResponseWriter, err error) bool
	// WriteSSEError sends an SSE error event (OpenAI-compatible error object
	// followed by [DONE]).
	WriteSSEError(gw *gateway.NenyaGateway, w http.ResponseWriter, statusCode int, message string)
	// WriteStructuredError writes a structured JSON error body.
	WriteStructuredError(w http.ResponseWriter, statusCode int, kind infra.ErrorKind, msg string)
}
