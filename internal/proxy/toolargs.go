package proxy

import (
	"log/slog"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/stream"
)

// toolArgsMaxBytes resolves the streamed tool-call assembler's per-call
// accumulation cap (NENYA-344): an operator-configured MCP tool-argument cap
// is the operator's declared bound for how many argument bytes are worth
// guarding, so the assembler aligns with it; otherwise the stream package
// default (aligned with mcp.DefaultMaxArgBytes) applies. The result is
// clamped to the stream-total bound — the constructor enforces the same
// clamp, so an operator cap above it can never punch through.
func toolArgsMaxBytes(gw *gateway.NenyaGateway) int {
	limit := stream.DefaultToolArgsMaxBytes
	if guard := gw.Config.Governance.MCPGuard; guard != nil && guard.MaxArgBytes > 0 {
		limit = guard.MaxArgBytes
	}
	if limit > stream.DefaultToolArgsTotalBytes {
		limit = stream.DefaultToolArgsTotalBytes
	}
	return limit
}

// toolArgsLogObserver is the Phase 010 production observer: it records
// reassembled tool-call activity — names and byte counts, never argument
// content — at Debug level, making the new observation surface visible in
// traces and giving the Phase 020 guard observers a registration pattern to
// extend. Fragment calls only accumulate per key; the per-call Debug line
// fires on completion so a hot stream pays one log per tool call, not per
// fragment.
type toolArgsLogObserver struct {
	logger   *slog.Logger
	fragLens map[string]int
}

// ToolArgsFragment accumulates the delivered byte count for one key. Logging
// happens on completion to keep per-fragment cost off the streaming hot path.
func (o *toolArgsLogObserver) ToolArgsFragment(key, name, fragment string) {
	o.fragLens[key] += len(fragment)
}

// ToolArgsComplete logs the completed observation for one tool call.
func (o *toolArgsLogObserver) ToolArgsComplete(key, name string) {
	o.logger.Debug("tool call arguments reassembled",
		"key", key, "name", name, "observed_bytes", o.fragLens[key])
	delete(o.fragLens, key)
}
