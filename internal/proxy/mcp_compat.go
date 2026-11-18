package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
	mcpproxy "github.com/nenya/internal/proxy/mcp"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
)

// ---- Type aliases: the MCP subsystem types moved to internal/proxy/mcp
// (package mcpproxy). The parent keeps the historical unexported spellings
// so every existing call site compiles unchanged.

type (
	chatRequest          = mcpproxy.ChatRequest
	httpError            = mcpproxy.HTTPError
	forwardOptions       = mcpproxy.ForwardOptions
	dispatchParams       = mcpproxy.DispatchParams
	streamResult         = mcpproxy.StreamResult
	upstreamAction       = mcpproxy.UpstreamAction
	egressScreenRuntime  = mcpproxy.EgressScreenRuntime
	bufferedSSE          = mcpproxy.BufferedSSE
	mcpToolCall          = mcpproxy.MCPToolCall
	immediateFlushWriter = mcpproxy.ImmediateFlushWriter
	mcpExecOptions       = mcpproxy.MCPExecOptions
	mcpGuardDispatch     = mcpproxy.MCPGuardDispatch
	mcpAppendContext     = mcpproxy.MCPAppendContext
	mcpIterInput         = mcpproxy.MCPIterInput
	bufferedDispatch     = mcpproxy.BufferedDispatch
	mcpVisionGate        = mcpproxy.VisionGate
	sseAccumulator       = mcpproxy.SSEAccumulator
	streamFailoverReason = mcpproxy.StreamFailoverReason
)

// retrySignal aliases the loop-control enum the moved buffered sweep shares
// with the chat retry loop.
type retrySignal = mcpproxy.RetrySignal

const (
	retrySignalContinue = mcpproxy.RetrySignalContinue
	retrySignalRetry    = mcpproxy.RetrySignalRetry
	retrySignalBreak    = mcpproxy.RetrySignalBreak
	retrySignalDone     = mcpproxy.RetrySignalDone
)

// Upstream action kinds moved with upstreamAction.
const (
	actionContinue = mcpproxy.ActionContinue
	actionError    = mcpproxy.ActionError
	actionStream   = mcpproxy.ActionStream
)

// streamFailoverReason moved with streamResult (aliased in the type block
// above).
const streamFailoverPolicyRefusal = mcpproxy.StreamFailoverPolicyRefusal

// Buffered sweep step constants (moved with the subsystem).
const (
	bufferedStepRetry = mcpproxy.BufferedStepRetry
)

// Constructor aliases for the moved buffered-core types.
var (
	newSSEAccumulator       = mcpproxy.NewSSEAccumulator
	newImmediateFlushWriter = mcpproxy.NewImmediateFlushWriter
)

// mcpExecTimeout bounds one MCP tool call dispatch. Single source is
// mcpproxy.MCPExecTimeout (the const moved with the subsystem); this alias
// keeps the auto-save path in stream.go on the same value.
const mcpExecTimeout = mcpproxy.MCPExecTimeout

// ---- proxyHost: the Host adapter delegating to the parent implementations.

// proxyHost adapts *Proxy to the mcpproxy.Host seam. Every method delegates
// to the existing parent implementation; the adapter carries no state.
type proxyHost struct {
	p *Proxy
}

// mcpHost returns the MCP subsystem's view of this proxy.
func (p *Proxy) mcpHost() mcpproxy.Host { return proxyHost{p} }

func (a proxyHost) APIKeyName(apiKey *config.ApiKey) string { return apiKeyName(apiKey) }

func (a proxyHost) ApplyToolDescriptionSpotlight(tools []map[string]any, settings pipeline.SpotlightSettings, metrics *infra.Metrics) {
	applyToolDescriptionSpotlight(tools, settings, metrics)
}

func (a proxyHost) ArmClientWriteDeadline(w http.ResponseWriter, timeout time.Duration) {
	ArmClientWriteDeadline(w, timeout)
}

func (a proxyHost) CalculateBackoff(attempt int) time.Duration { return calculateBackoff(attempt) }

func (a proxyHost) ExfilGuardFor(gw *gateway.NenyaGateway, agentName string) *stream.ExfilGuard {
	return exfilGuardFor(gw, agentName)
}

func (a proxyHost) HandleUpstreamError(gw *gateway.NenyaGateway, idx int, targets []routing.UpstreamTarget, target routing.UpstreamTarget, cooldownDuration time.Duration, agentName string, action mcpproxy.UpstreamAction) (mcpproxy.RetrySignal, time.Duration, bool) {
	return a.p.handleUpstreamError(gw, idx, targets, target, cooldownDuration, agentName, action)
}

func (a proxyHost) MatchRequestScopedError(gw *gateway.NenyaGateway, providerName string, statusCode int, body []byte) *config.RequestScopedErrorRule {
	return a.p.matchRequestScopedError(gw, providerName, statusCode, body)
}

func (a proxyHost) PrepareAndSend(gw *gateway.NenyaGateway, r *http.Request, dp mcpproxy.DispatchParams) mcpproxy.UpstreamAction {
	return a.p.prepareAndSend(gw, r, dp)
}

func (a proxyHost) RecordBufferedOutcome(gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string, dispatchAt, bodyReadAt time.Time, cooldown time.Duration) {
	recordBufferedOutcome(gw, target, agentName, dispatchAt, bodyReadAt, cooldown)
}

func (a proxyHost) RecordChatUsageTotals(gw *gateway.NenyaGateway, model string, totals stream.UsageTotals) {
	recordChatUsageTotals(gw, model, totals)
}

func (a proxyHost) RecordCostAndBilling(ctx context.Context, gw *gateway.NenyaGateway, target routing.UpstreamTarget, servedModel string, inputTokens, cachedInputTokens, cacheWriteTokens, outputTokens int) {
	recordCostAndBilling(ctx, gw, target, servedModel, inputTokens, cachedInputTokens, cacheWriteTokens, outputTokens)
}

func (a proxyHost) RecordNonStreamingMetrics(gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string, outputTokens, cacheHitTokens, cacheMissTokens, cacheCreationTokens, reasoningTokens int) {
	recordNonStreamingMetrics(gw, target, agentName, outputTokens, cacheHitTokens, cacheMissTokens, cacheCreationTokens, reasoningTokens)
}

func (a proxyHost) RecordNonStreamingStats(gw *gateway.NenyaGateway, model string, outputTokens, cacheHitTokens, cacheMissTokens, cacheCreationTokens, reasoningTokens int) {
	recordNonStreamingStats(gw, model, outputTokens, cacheHitTokens, cacheMissTokens, cacheCreationTokens, reasoningTokens)
}

func (a proxyHost) ReserveKeyBudget(gw *gateway.NenyaGateway, key *config.ApiKey, tokenCount int) (string, bool) {
	return reserveKeyBudget(gw, key, tokenCount)
}

func (a proxyHost) ResolveStreamStallTimeout(gw *gateway.NenyaGateway) time.Duration {
	return resolveStreamStallTimeout(gw)
}

func (a proxyHost) ResolveTransformer(gw *gateway.NenyaGateway, target routing.UpstreamTarget, sourceFormat string, sigCache infra.ThoughtSignatureKV) stream.ResponseTransformer {
	return a.p.resolveTransformer(gw, target, sourceFormat, sigCache)
}

func (a proxyHost) ScanToolArgsCanary(canary pipeline.CanarResult, args map[string]any, metrics *infra.Metrics, logger *slog.Logger, tool string) (tripped, refuse bool) {
	return scanToolArgsCanary(canary, args, metrics, logger, tool)
}

func (a proxyHost) ScopedThoughtSigCache(gw *gateway.NenyaGateway, apiKey *config.ApiKey) infra.ThoughtSignatureKV {
	return scopedThoughtSigCache(gw, apiKey)
}

func (a proxyHost) SpotlightSettingsFor(cfg *config.Config, agent config.AgentConfig) pipeline.SpotlightSettings {
	return spotlightSettingsFor(cfg, agent)
}

func (a proxyHost) WaitWithCancel(ctx context.Context, d time.Duration) { waitWithCancel(ctx, d) }

func (a proxyHost) WriteExfilBlockedSSE(gw *gateway.NenyaGateway, w http.ResponseWriter) {
	a.p.writeExfilBlockedSSE(gw, w)
}

func (a proxyHost) WriteExfilDetectedSSE(gw *gateway.NenyaGateway, w http.ResponseWriter) {
	a.p.writeExfilDetectedSSE(gw, w)
}

func (a proxyHost) WriteGatewayStreamError(w http.ResponseWriter, statusCode int, errType infra.ErrorType, message string) {
	writeGatewayStreamError(w, statusCode, ErrorType(errType), message)
}

func (a proxyHost) WritePipelineRejection(gw *gateway.NenyaGateway, w http.ResponseWriter, err error) bool {
	return writePipelineRejection(gw, w, err)
}

func (a proxyHost) WriteSSEError(gw *gateway.NenyaGateway, w http.ResponseWriter, statusCode int, message string) {
	writeSSEError(gw, w, statusCode, message)
}

func (a proxyHost) WriteStructuredError(w http.ResponseWriter, statusCode int, kind infra.ErrorKind, msg string) {
	writeStructuredError(w, statusCode, kind, msg)
}

// ---- Test shims: the parent tests call the moved functions as *Proxy
// methods. These thin delegators keep those call sites compiling unchanged
// (field spellings aside). They are not used by production code.

func (p *Proxy) injectMCPTools(gw *gateway.NenyaGateway, payload map[string]interface{}, req *chatRequest) bool {
	return mcpproxy.InjectMCPTools(p.mcpHost(), gw, payload, req)
}

func (p *Proxy) checkMCPAvailability(gw *gateway.NenyaGateway, req *chatRequest) *httpError {
	return mcpproxy.CheckMCPAvailability(p.mcpHost(), gw, req)
}

func (p *Proxy) injectAutoSearch(gw *gateway.NenyaGateway, ctx context.Context, payload map[string]interface{}, messages []interface{}, req *chatRequest) {
	mcpproxy.InjectAutoSearch(p.mcpHost(), gw, ctx, payload, messages, req)
}

// discoverToolByPrefix resolves an MCP tool by name prefix. Single shim
// serves both the auto-save path in stream.go and the test helpers.
func (p *Proxy) discoverToolByPrefix(gw *gateway.NenyaGateway, serverName, prefix string) string {
	return mcpproxy.DiscoverToolByPrefix(p.mcpHost(), gw, serverName, prefix)
}

// Test shims for auto-search helpers moved with the subsystem.

// writeSSEError is the parent-side SSE error writer shared by the chat
// streaming path and the MCP subsystem (via mcpproxy.Host.WriteSSEError).
func writeSSEError(gw *gateway.NenyaGateway, w http.ResponseWriter, statusCode int, message string) {
	// NENYA-155: re-arm the client-stall deadline — a terminal written long
	// after the last streamed chunk must not be silently dropped by the stale
	// deadline, and must stay bounded against a stuck peer.
	ArmClientWriteDeadline(w, resolveStreamStallTimeout(gw))
	writeGatewayStreamError(w, statusCode, ErrorTypeGateway, message)
}

// detectRequestCapabilities stays referenced from chat.go; it moved with the
// subsystem and is re-exported here for the parent call site.
var detectRequestCapabilities = mcpproxy.DetectRequestCapabilities

// guardMCPArgs delegates to the moved implementation (auto-save call site in
// stream.go).
var guardMCPArgs = mcpproxy.GuardMCPArgs

// Follow-up note removed (NENYA-316): Host.ScreenToolArgs was a method on
// EgressScreenRuntime, not a host capability — callers hold the runtime
// (opts.Screen) and call it directly; the nil receiver fails closed.
