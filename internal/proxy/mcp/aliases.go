package mcpproxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/mcp"
	"github.com/nenya/internal/pipeline"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
)

// Internal aliases: the moved bodies keep their original unexported
// spellings, so the extraction diff stays minimal. Everything aliases the
// exported names the parent package uses through its own aliases.

type (
	mcpIterInput     = MCPIterInput
	chatRequest      = ChatRequest
	httpError        = HTTPError
	bufferedSSE      = BufferedSSE
	mcpToolCall      = MCPToolCall
	mcpVisionGate    = VisionGate
	visionDrop       = VisionDrop
	mcpExecOptions   = MCPExecOptions
	mcpGuardDispatch = MCPGuardDispatch
	mcpAppendContext = MCPAppendContext
	autoSearchResult = AutoSearchResult
)

const (
	actionContinue = ActionContinue
	actionError    = ActionError
	actionStream   = ActionStream

	retrySignalContinue = RetrySignalContinue
	retrySignalRetry    = RetrySignalRetry
	retrySignalBreak    = RetrySignalBreak
	retrySignalDone     = RetrySignalDone
)

// Unexported field aliases via accessor shims are not possible in Go; the
// moved bodies use the exported field spellings directly.

// Exported aliases for the loop-internal types the parent's compat layer
// references in its test shims.

// BufferedDispatch is the exported spelling of the buffered dispatch
// parameters bundle (parent alias: bufferedDispatch).
type BufferedDispatch = bufferedDispatch

// BufferedOutcome is the exported spelling of the buffered sweep's decision
// record (parent alias: bufferedOutcome).
type BufferedOutcome = bufferedOutcome

// GuardMCPArgs is the exported spelling of the argument guard entry point.
func GuardMCPArgs(gw *gateway.NenyaGateway, logger *slog.Logger, d MCPGuardDispatch) error {
	return guardMCPArgs(gw, logger, d)
}

// DetectRequestCapabilities inspects a request payload for tool/vision/
// reasoning capabilities.
func DetectRequestCapabilities(payload map[string]interface{}) routing.RequestCapabilities {
	return detectRequestCapabilities(payload)
}

// RescanAppendedMessages runs the deterministic security interceptors over
// the tool-result messages the loop just appended.
func RescanAppendedMessages(h Host, in MCPIterInput, working map[string]any, appended []map[string]any) error {
	return rescanAppendedMessages(h, in, working, appended)
}

// ExecuteAndAppendMCPResults executes the intercepted MCP tool calls and
// appends their results to the working conversation.
func ExecuteAndAppendMCPResults(h Host, in MCPIterInput, working map[string]any, mcpCalls []MCPToolCall, buf *BufferedSSE) bool {
	return executeAndAppendMCPResults(h, in, working, mcpCalls, buf)
}

// DiscoverToolByPrefix resolves one server's tool whose name contains the
// prefix (auto-save tool resolution).
func DiscoverToolByPrefix(h Host, gw *gateway.NenyaGateway, serverName, prefix string) string {
	return discoverToolByPrefix(h, gw, serverName, prefix)
}

// BufferStreamResponse buffers one upstream SSE stream into a BufferedSSE.
func BufferStreamResponse(ctx context.Context, r io.Reader, logger *slog.Logger, servingModel string, servingTarget routing.UpstreamTarget) (*BufferedSSE, error) {
	return bufferStreamResponse(ctx, r, logger, servingModel, servingTarget)
}

// InspectMessageCaps inspects a message object and updates the request
// capabilities (exported for the parent's capability tests).
func InspectMessageCaps(msg any, caps *routing.RequestCapabilities) bool {
	return inspectMessageCaps(msg, caps)
}

// PrepareOriginalPayload marshals the payload (and minifies when compaction
// is configured); exported for the parent's helper tests.
func PrepareOriginalPayload(gw *gateway.NenyaGateway, payload map[string]interface{}) ([]byte, error) {
	return prepareOriginalPayload(gw, payload)
}

// Exported wrappers for the parent's auto-search helper tests.

// ExtractAutoSearchQuery pulls the auto-search query terms from the request's
// most recent user message (exported for the parent test shims).
func ExtractAutoSearchQuery(h Host, messages []interface{}) (string, map[string]interface{}) {
	return extractAutoSearchQuery(h, messages)
}

// ResolveSearchTool resolves the configured (or prefixed) search tool for a
// server's auto-search flow.
func ResolveSearchTool(h Host, gw *gateway.NenyaGateway, serverName, configuredTool, agentName string) string {
	return resolveSearchTool(h, gw, serverName, configuredTool, agentName)
}

// CanPerformAutoSearch reports whether a server's auto-search is configured
// and currently usable.
func CanPerformAutoSearch(h Host, gw *gateway.NenyaGateway, serverName string) bool {
	return canPerformAutoSearch(h, gw, serverName)
}

// MCPClientCallTool dispatches one MCP tools/call request through the
// gateway's MCP client (auto-search path).
func MCPClientCallTool(h Host, gw *gateway.NenyaGateway, ctx context.Context, serverName, toolName, query string, apiKey *config.ApiKey) (*mcp.CallToolResult, error) {
	return mcpClientCallTool(h, gw, ctx, serverName, toolName, query, apiKey)
}

// Exported wrappers for parent-package tests over moved package-level helpers.

// MCPCalibrationModel names the model token calibration is recorded against
// for a buffered response (the serving target's model).
func MCPCalibrationModel(targets []routing.UpstreamTarget, buf *BufferedSSE) string {
	return mcpCalibrationModel(targets, buf)
}

// RecordCalibrationFromBuffer feeds a buffered response's usage into the
// token-calibration loop.
func RecordCalibrationFromBuffer(gw *gateway.NenyaGateway, model string, buf *BufferedSSE) {
	recordCalibrationFromBuffer(gw, model, buf)
}

// ReplayBufferedResponse writes a buffered response back to the client in
// the client's wire format (conversion + transformer held-tail append).
func ReplayBufferedResponse(h Host, w http.ResponseWriter, buf *BufferedSSE, gw *gateway.NenyaGateway, providerName string, logger *slog.Logger) {
	replayBufferedResponse(h, w, buf, gw, providerName, logger)
}

// BuildOpenAIToolCalls renders extracted tool calls as OpenAI tool_calls
// objects for the replay conversion.
func BuildOpenAIToolCalls(calls []MCPToolCall) []any { return buildOpenAIToolCalls(calls) }

// PartitionMCPToolCalls splits extracted calls into MCP-routed and plain
// tool_calls using the gateway's tool registry.
func PartitionMCPToolCalls(calls []MCPToolCall, toolIndex *mcp.ToolRegistry) (mcpCalls, nonMcpCalls []MCPToolCall) {
	return partitionMCPToolCalls(calls, toolIndex)
}

// InjectedToolNameSet builds the trust-anchor set of injected tool names
// (NENYA-162 non-injected-call guard).
func InjectedToolNameSet(names []string) map[string]struct{} { return injectedToolNameSet(names) }

// UnavailableMCPToolCalls reports which extracted calls name tools outside
// the injected set (the refusal candidates).
func UnavailableMCPToolCalls(calls []MCPToolCall, injected map[string]struct{}) map[string]bool {
	return unavailableMCPToolCalls(calls, injected)
}

// ExecuteMCPCallsWithUnavailable executes the MCP-routed calls and returns
// structured [unavailable tool] IsError results for non-injected names
// (NENYA-162: refuse, never silently drop).
func ExecuteMCPCallsWithUnavailable(h Host, in MCPExecOptions) []*mcp.CallToolResult {
	return executeMCPCallsWithUnavailable(h, in)
}

// AppendMCPResults folds executed tool results into the working payload as
// tool-role messages (spotlight/datamarking applied per MCPAppendContext).
func AppendMCPResults(payload map[string]any, calls []MCPToolCall, results []*mcp.CallToolResult, assistantMsg map[string]any, opts MCPAppendContext) []map[string]any {
	return appendMCPResults(payload, calls, results, assistantMsg, opts)
}

// ToolResultContent renders one tool result for the payload: text plus
// vision-gated image blocks (NENYA-149), reporting dropped images.
func ToolResultContent(text string, result *mcp.CallToolResult, gate VisionGate, logger *slog.Logger) (any, []VisionDrop) {
	return toolResultContent(text, result, gate, logger)
}

// RecordVisionDrops counts vision-gated image drops per reason/action
// (nenya_vision_content_dropped_total).
func RecordVisionDrops(metrics *infra.Metrics, drops []VisionDrop) { recordVisionDrops(metrics, drops) }

// ApplySpotlightToParts envelopes spotlighted content parts (server/tool
// attribution) in a tool result.
func ApplySpotlightToParts(content any, settings pipeline.SpotlightSettings, server, tool string) (any, bool) {
	return applySpotlightToParts(content, settings, server, tool)
}

// Sentinel errors moved with the subsystem; parent tests reference them.
var (
	ErrMCPNotGranted        = errMCPNotGranted
	ErrMCPServerUnavailable = errMCPServerUnavailable
	ErrMCPArgumentPolicy    = errMCPArgumentPolicy
)

// Exported wrappers for the parent's tool-policy/search tests.

// MatchesToolPattern is the exported spelling of the tool-pattern matcher.
func MatchesToolPattern(name, pattern string) bool { return matchesToolPattern(name, pattern) }

// ServerToolPolicy applies a server's tool_include/tool_exclude globs to a
// granted tool list (exclude wins).
func ServerToolPolicy(tools []mcp.Tool, include, exclude []string) []mcp.Tool {
	return serverToolPolicy(tools, include, exclude)
}

// SelectToolsForRequest ranks the catalog by conversation relevance and
// returns the top-N tools plus the configured floor (NENYA-162).
func SelectToolsForRequest(tools []mcp.Tool, query string, topN int, floor []string) ([]mcp.Tool, string) {
	return selectToolsForRequest(tools, query, topN, floor)
}

// Exported buffered sweep steps live in mcp_loop.go (BufferedStep*). The
// parent keeps the historical lowercase spellings via mcp_compat.go, and
// the package-internal bodies keep their unexported spellings here.
const (
	bufferedStepNext  = BufferedStepNext
	bufferedStepRetry = BufferedStepRetry
	bufferedStepStop  = BufferedStepStop
)
