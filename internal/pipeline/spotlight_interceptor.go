package pipeline

import (
	"context"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
)

// SpotlightInterceptor envelopes incoming role:"tool" messages in
// untrusted-content delimiters (Microsoft spotlighting, arXiv:2403.14720 —
// indirect-injection ASR above 50% down to under 2%). This is the primary
// defense surface for client-side MCP setups (opencode/crush): their tool
// results arrive as tool-role messages in subsequent requests and transit
// the gateway here. Priority: 12 — after pattern redaction (10) so
// envelopes wrap already-redacted content, and before injection detection
// (15), which still scans the enveloped text. Delimiters mode only:
// datamarking would corrupt code fidelity the model must quote and edit.
// The transform is deterministic, keeping provider prompt-cache prefixes
// stable across requests.
type SpotlightInterceptor struct {
	name         string
	priority     int
	global       config.SpotlightConfig
	agentHistory map[string]*bool
	agentEnabled map[string]*bool
	metrics      *infra.Metrics
}

// NewSpotlightInterceptor snapshots the global config and per-agent
// overrides (enabled/history-enabled are the per-agent surface).
func NewSpotlightInterceptor(cfg *config.SpotlightConfig, agents map[string]config.AgentConfig, metrics *infra.Metrics) *SpotlightInterceptor {
	if cfg == nil {
		cfg = &config.SpotlightConfig{}
	}
	agentEnabled := make(map[string]*bool, len(agents))
	agentHistory := make(map[string]*bool, len(agents))
	for name, agent := range agents {
		if agent.Spotlight == nil {
			continue
		}
		if agent.Spotlight.Enabled != nil {
			agentEnabled[name] = agent.Spotlight.Enabled
		}
		if agent.Spotlight.HistoryEnabled != nil {
			agentHistory[name] = agent.Spotlight.HistoryEnabled
		}
	}
	return &SpotlightInterceptor{
		name:         "spotlight",
		priority:     12,
		global:       *cfg,
		agentEnabled: agentEnabled,
		agentHistory: agentHistory,
		metrics:      metrics,
	}
}

func (s *SpotlightInterceptor) Name() string  { return s.name }
func (s *SpotlightInterceptor) Priority() int { return s.priority }

// RegistrationRequired reports whether any surface could apply: the
// global spotlight is enabled (history follows enabled by default) or any
// agent overrides enabled/history on.
func (s *SpotlightInterceptor) RegistrationRequired() bool {
	if s.global.Enabled != nil && *s.global.Enabled {
		return true
	}
	if s.global.HistoryEnabled != nil && *s.global.HistoryEnabled {
		return true
	}
	for _, enabled := range s.agentEnabled {
		if enabled != nil && *enabled {
			return true
		}
	}
	for _, history := range s.agentHistory {
		if history != nil && *history {
			return true
		}
	}
	return false
}

// resolveSettings applies per-agent overrides on top of the global config
// for the agent named in the payload's model field. History resolution
// order: agent history_enabled → agent enabled (an agent opting into
// spotlight inherits history with it) → global history_enabled (which
// defaults to global enabled via config defaults).
func (s *SpotlightInterceptor) resolveSettings(req *InterceptRequest) (historyEnabled bool) {
	historyEnabled = s.global.HistoryEnabled != nil && *s.global.HistoryEnabled
	agentName, _ := req.Payload["model"].(string)
	if override, ok := s.agentHistory[agentName]; ok && override != nil {
		historyEnabled = *override
	} else if override, ok := s.agentEnabled[agentName]; ok && override != nil {
		historyEnabled = *override
	}
	if override, ok := s.agentEnabled[agentName]; ok && override != nil && !*override {
		// Explicit per-agent disable wins over every inheritance path.
		historyEnabled = false
	}
	return historyEnabled
}

func (s *SpotlightInterceptor) CanHandle(ctx context.Context, req *InterceptRequest) bool {
	if ctx.Err() != nil {
		return false
	}
	if !s.resolveSettings(req) {
		return false
	}
	for _, msg := range req.Messages {
		if role, _ := msg["role"].(string); role == "tool" {
			return true
		}
	}
	return false
}

func (s *SpotlightInterceptor) Process(ctx context.Context, req *InterceptRequest) (*InterceptResult, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !s.resolveSettings(req) {
		return &InterceptResult{Payload: req.Payload, Skip: true}, nil
	}

	modified := false
	preamblePending := true
	for _, msg := range req.Messages {
		if role, _ := msg["role"].(string); role != "tool" {
			continue
		}
		wrapped := false
		redact := func(content string) string {
			// Always re-envelope: a Contains skip would let attackers
			// forge envelope tags in tool output to slip content through
			// unenveloped. Stacked envelopes are bounded because
			// ApplyHistory defangs the lookalikes it wraps.
			wrapped = true
			out := spotlightWithPreamble(content, preamblePending)
			// Flip on first invocation: exactly one rule per request,
			// covering multi-part messages too.
			preamblePending = false
			return out
		}
		WalkMessageText(msg, redact)
		if wrapped {
			modified = true
			s.metrics.RecordSpotlighted("tool-history")
		}
	}

	if !modified {
		return &InterceptResult{Payload: req.Payload, Skip: true}, nil
	}
	return &InterceptResult{
		Payload:   req.Payload,
		Truncated: false,
		Reason:    "spotlighted",
	}, nil
}
