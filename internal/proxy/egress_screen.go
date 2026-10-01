package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
)

// egressScreenRuntime carries the per-request egress screen state: the
// judge (nil = screen disabled), whether strict action is configured,
// and the one-judgment-per-request budget. The runtime is created once
// per request in handleChatCompletions and threaded through the forward
// options next to the canary result, so the tool-args and buffered
// checkpoints share the same single-judgment budget.
type egressScreenRuntime struct {
	judge  *pipeline.Judge
	strict bool
	// budget flips to true when the single judgment is spent; later
	// triggers in the same request keep their deterministic verdict.
	budget atomic.Bool
}

// egressScreenFor resolves the egress screen for a request from
// governance.judgments[egress_screen]: enabled toggles the site, action
// "strict" upgrades a screen-exfil verdict to a block, and the engine
// comes from the entry (inheriting the escalation/bouncer chain).
// Returns nil when disabled or unconfigured — the zero-cost fast path.
func egressScreenFor(gw *gateway.NenyaGateway) *egressScreenRuntime {
	jc := gw.Config.Governance.Judgments[pipeline.EgressScreenJudgmentName]
	if jc == nil || jc.Enabled == nil || !*jc.Enabled || gw.EgressScreenJudge == nil {
		return nil
	}
	return &egressScreenRuntime{judge: gw.EgressScreenJudge, strict: jc.Action == "strict"}
}

// screenOnce adjudicates one piece of flagged egress content when the
// per-request budget is unspent. Returns true only for a contract-valid
// exfil verdict; every other outcome (benign, inconclusive, budget
// spent, operational failure) is false and the caller keeps its
// deterministic verdict — the screen can only strengthen, never weaken.
// Empty content returns false WITHOUT spending the budget; an attempted
// adjudication counts as spent even when it fails.
func (s *egressScreenRuntime) screenOnce(ctx context.Context, agentName, content string) bool {
	if s == nil || s.judge == nil || content == "" {
		return false
	}
	if !s.budget.CompareAndSwap(false, true) {
		return false
	}
	res := s.judge.Adjudicate(ctx, agentName, content)
	return res.OK && res.Verdict == pipeline.VerdictExfil
}

// recordScreenDetection records the screen outcome in the exfil
// detection counter under the llm_screen reason, with the action the
// gateway applied (block under strict, log otherwise).
func (s *egressScreenRuntime) recordDetection(metrics *infra.Metrics) {
	action := "log"
	if s != nil && s.strict {
		action = "block"
	}
	metrics.RecordExfilDetection("llm_screen", action)
}

// logScreenHit emits the shared warn line for a screen-exfil verdict
// (action labels match the metric: block|log).
func (s *egressScreenRuntime) logScreenHit(logger *slog.Logger, agentName, surface string) {
	action := "log"
	if s != nil && s.strict {
		action = "block"
	}
	logger.Warn("egress screen: exfiltration verdict on flagged output",
		"agent", agentName, "surface", surface, "action", action)
}

// screenToolArgs runs the egress screen over serialized tool arguments
// when a deterministic trigger fired. Returns true when the strict
// screen refuses the call; every other outcome (disabled, spent budget,
// benign/inconclusive/failure verdict, non-strict action) leaves the
// deterministic verdict in place.
func (s *egressScreenRuntime) screenToolArgs(ctx context.Context, gw *gateway.NenyaGateway, agentName string, args map[string]any, triggered bool) bool {
	if s == nil || !triggered {
		return false
	}
	// Serialize the structured arguments for adjudication;
	// unserializable arguments skip the screen (the Judge can only
	// strengthen, so skipping keeps the deterministic verdict).
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return false
	}
	if !s.screenOnce(ctx, agentName, string(argsJSON)) {
		return false
	}
	s.recordDetection(gw.Metrics)
	s.logScreenHit(gw.Logger, agentName, "tool_args")
	return s.strict
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
