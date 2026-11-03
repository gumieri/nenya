package mcpproxy

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
)

// ScreenOnce adjudicates one piece of flagged egress content when the
// per-request budget is unspent. Returns true only for a contract-valid
// exfil verdict; every other outcome (benign, inconclusive, budget
// spent, operational failure) is false and the caller keeps its
// deterministic verdict — the screen can only strengthen, never weaken.
// Empty content returns false WITHOUT spending the budget; an attempted
// adjudication counts as spent even when it fails.
func (s *EgressScreenRuntime) ScreenOnce(ctx context.Context, agentName, content string) bool {
	if s == nil || s.Judge == nil || content == "" {
		return false
	}
	if !s.Budget.CompareAndSwap(false, true) {
		return false
	}
	res := s.Judge.Adjudicate(ctx, agentName, content)
	return res.OK && res.Verdict == pipeline.VerdictExfil
}

// RecordDetection records the screen outcome in the exfil
// detection counter under the llm_screen reason, with the action the
// gateway applied (block under strict, log otherwise).
func (s *EgressScreenRuntime) RecordDetection(metrics *infra.Metrics) {
	action := "log"
	if s != nil && s.Strict {
		action = "block"
	}
	metrics.RecordExfilDetection("llm_screen", action)
}

// LogScreenHit emits the shared warn line for a screen-exfil verdict
// (action labels match the metric: block|log).
func (s *EgressScreenRuntime) LogScreenHit(logger *slog.Logger, agentName, surface string) {
	action := "log"
	if s != nil && s.Strict {
		action = "block"
	}
	logger.Warn("egress screen: exfiltration verdict on flagged output",
		"agent", agentName, "surface", surface, "action", action)
}

// ScreenToolArgs runs the egress screen over serialized tool arguments
// when a deterministic trigger fired. Returns true when the strict
// screen refuses the call; every other outcome (disabled, spent budget,
// benign/inconclusive/failure verdict, non-strict action) leaves the
// deterministic verdict in place.
func (s *EgressScreenRuntime) ScreenToolArgs(ctx context.Context, gw *gateway.NenyaGateway, agentName string, args map[string]any, triggered bool) bool {
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
	if !s.ScreenOnce(ctx, agentName, string(argsJSON)) {
		return false
	}
	s.RecordDetection(gw.Metrics)
	s.LogScreenHit(gw.Logger, agentName, "tool_args")
	return s.Strict
}
