package mcpproxy

import (
	"context"
	"io"
	"log/slog"

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

	retrySignalRetry = RetrySignalRetry
	retrySignalDone  = RetrySignalDone
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

// DiscoverToolByPrefix resolves one server's tool whose name contains the
// prefix (auto-save tool resolution).
func DiscoverToolByPrefix(h Host, gw *gateway.NenyaGateway, serverName, prefix string) string {
	return discoverToolByPrefix(h, gw, serverName, prefix)
}

// BufferStreamResponse buffers one upstream SSE stream into a BufferedSSE.
func BufferStreamResponse(ctx context.Context, r io.Reader, logger *slog.Logger, servingModel string, servingTarget routing.UpstreamTarget) (*BufferedSSE, error) {
	return bufferStreamResponse(ctx, r, logger, servingModel, servingTarget)
}

// Sentinel errors moved with the subsystem; parent tests reference them.
var (
	ErrMCPNotGranted        = errMCPNotGranted
	ErrMCPServerUnavailable = errMCPServerUnavailable
	ErrMCPArgumentPolicy    = errMCPArgumentPolicy
)

// Exported buffered sweep steps live in mcp_loop.go (BufferedStep*). The
// parent keeps the historical lowercase spellings via mcp_compat.go, and
// the package-internal bodies keep their unexported spellings here.
const (
	bufferedStepNext  = BufferedStepNext
	bufferedStepRetry = BufferedStepRetry
)
