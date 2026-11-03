package proxy

import (
	"log/slog"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
)

// egressScreenFor resolves the egress screen for a request from
// governance.judgments[egress_screen]: enabled toggles the site, action
// "strict" upgrades a screen-exfil verdict to a block, and the engine
// comes from the entry (inheriting the escalation/bouncer chain).
// Returns nil when disabled or unconfigured — the zero-cost fast path.
// The runtime type and its methods moved with the MCP subsystem
// (mcpproxy.EgressScreenRuntime); the chat path shares them via the alias.
func egressScreenFor(gw *gateway.NenyaGateway) *egressScreenRuntime {
	jc := gw.Config.Governance.Judgments[pipeline.EgressScreenJudgmentName]
	if jc == nil || jc.Enabled == nil || !*jc.Enabled || gw.EgressScreenJudge == nil {
		return nil
	}
	return &egressScreenRuntime{Judge: gw.EgressScreenJudge, Strict: jc.Action == "strict"}
}

// scanToolArgsCanary applies the canary tripwire to outbound tool
// arguments before dispatch, recording channel events. Returns whether
// the canary tripped (an egress-screen trigger) and whether the call
// must be refused; unscannable arguments are reported for observability
// only (they refuse under block mode, and cannot be adjudicated).
func scanToolArgsCanary(canary pipeline.CanarResult, args map[string]any, metrics *infra.Metrics, logger *slog.Logger, tool string) (tripped, refuse bool) {
	tripped, unscannable, refuse := canary.ScanArgs(args)
	if tripped {
		metrics.RecordExfilEvent("tool_args")
		logger.Warn("canary detected in MCP tool arguments",
			"tool", tool, "channel", "tool_args")
	}
	if unscannable {
		metrics.RecordExfilEvent("tool_args")
		logger.Warn("MCP tool arguments unscannable; skipped by canary scan",
			"tool", tool)
	}
	return tripped, refuse
}
