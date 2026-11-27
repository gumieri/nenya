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

// toolArgsFanOut fans fragments out to the Phase 010 log observer and the
// Phase 020 enforcing guard observer. Nil members are skipped; a guard
// verdict terminates the stream via the assembler regardless of delivery
// order.
type toolArgsFanOut struct {
	activity *toolArgsLogObserver
	guards   *stream.ToolArgsGuardObserver
}

// ToolArgsFragment fans one fragment out to every registered observer.
func (f *toolArgsFanOut) ToolArgsFragment(key, name, fragment string) {
	if f.activity != nil {
		f.activity.ToolArgsFragment(key, name, fragment)
	}
	if f.guards != nil {
		f.guards.ToolArgsFragment(key, name, fragment)
	}
}

// ToolArgsComplete fans one completion out to every registered observer.
func (f *toolArgsFanOut) ToolArgsComplete(key, name string) {
	if f.activity != nil {
		f.activity.ToolArgsComplete(key, name)
	}
	if f.guards != nil {
		f.guards.ToolArgsComplete(key, name)
	}
}

// toolArgsGuards builds the Phase 020 enforcing observer for the streamed
// tool-call arguments: dedicated ExfilGuard and CanaryWatcher instances
// (their sliding windows must hold argument bytes, not text-path bytes) with
// the same effective config as their text-path counterparts. Both nil when
// disabled — the zero-cost path.
func toolArgsGuards(assembler *stream.ToolCallAssembler, gw *gateway.NenyaGateway, o transformReaderOpts) *stream.ToolArgsGuardObserver {
	if assembler == nil {
		return nil
	}
	logger := gw.Logger.With("agent", o.agentName, "provider", o.target.Provider, "model", o.target.Model, "channel", "tool_args")
	var exfil *stream.ExfilGuard
	// A dedicated instance per channel: the text-path guard built at the
	// SetExfilGuard site holds forwarded text in its window; this one
	// holds argument bytes. Same effective config, separate window.
	if guard := exfilGuardFor(gw, o.agentName); guard != nil {
		exfil = guard
	}
	var canary *stream.CanaryWatcher
	if o.canary.Token != "" {
		canary = stream.NewCanaryWatcher(o.canary.Token, o.canary.Action, "tool_args", gw.Metrics, logger)
	}
	if exfil == nil && canary == nil {
		return nil
	}
	return stream.NewToolArgsGuardObserver(assembler, exfil, canary, logger)
}
