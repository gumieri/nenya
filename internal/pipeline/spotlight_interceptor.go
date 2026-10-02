package pipeline

import (
	"context"
	"io"
	"log/slog"
	"strings"

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
// (15), which still scans the enveloped text.
//
// In blanket mode (risk tiers disabled) the transform is delimiters-only
// and deterministic, keeping provider prompt-cache prefixes stable across
// requests — datamarking would corrupt code fidelity the model must
// quote and edit.
//
// With governance.spotlight.risk_tiers enabled, blanket delimiters are
// replaced by risk-tiered marking: concrete markers (instruction
// overrides, credential shapes, role forgery) force the high tier
// (datamarking inside the envelope); plain local output stays low
// (delimiters, historical behavior); web sources and oversized texts are
// the ambiguous band, resolved by the advisory judgment and failing
// closed to high.
type SpotlightInterceptor struct {
	name         string
	priority     int
	global       config.SpotlightConfig
	agentHistory map[string]*bool
	agentEnabled map[string]*bool
	metrics      *infra.Metrics
	logger       *slog.Logger
	tierJudge    *Judge
}

// NewSpotlightInterceptor snapshots the global config and per-agent
// overrides (enabled/history-enabled are the per-agent surface). The
// tier judgment is attached via SetTierJudge after construction (only
// when the interceptor registers) — nil keeps blanket delimiters.
func NewSpotlightInterceptor(cfg *config.SpotlightConfig, agents map[string]config.AgentConfig, metrics *infra.Metrics, logger *slog.Logger) *SpotlightInterceptor {
	if cfg == nil {
		cfg = &config.SpotlightConfig{}
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
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
		logger:       logger,
	}
}

// SetTierJudge attaches the advisory spotlight-tier judge (nil =
// ambiguous bands fail closed to high deterministically). Must be
// called before the interceptor serves.
func (s *SpotlightInterceptor) SetTierJudge(judge *Judge) { s.tierJudge = judge }

func (s *SpotlightInterceptor) Name() string  { return s.name }
func (s *SpotlightInterceptor) Priority() int { return s.priority }

// Strict implements StrictInterceptor: enveloping is a security surface —
// an operational failure must not forward unenveloped tool output.
func (s *SpotlightInterceptor) Strict() bool { return true }

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
// for the canonical agent identity: the proxy-resolved Agent config when
// present, otherwise the name-keyed snapshot lookup via AgentNameFor.
// History resolution
// order: agent history_enabled → agent enabled (an agent opting into
// spotlight inherits history with it) → global history_enabled (which
// defaults to global enabled via config defaults).
func (s *SpotlightInterceptor) resolveSettings(req *InterceptRequest) (historyEnabled bool) {
	historyEnabled = s.global.HistoryEnabled != nil && *s.global.HistoryEnabled
	if req.Agent != nil && req.Agent.Spotlight != nil {
		ov := req.Agent.Spotlight
		if ov.HistoryEnabled != nil {
			historyEnabled = *ov.HistoryEnabled
		} else if ov.Enabled != nil {
			historyEnabled = *ov.Enabled
		}
		if ov.Enabled != nil && !*ov.Enabled {
			// Explicit per-agent disable wins over every inheritance path.
			historyEnabled = false
		}
		return historyEnabled
	}
	agentName := AgentNameFor(req)
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
	agentName := AgentNameFor(req)
	// One judgment per request for the ambiguous band: later ambiguous
	// messages fail closed to high (escalation-style budget).
	judgeBudget := 1
	for _, msg := range req.Messages {
		if role, _ := msg["role"].(string); role != "tool" {
			continue
		}
		tier, source := s.resolveTier(ctx, agentName, msg, &judgeBudget)
		wrapped := false
		redact := func(content string) string {
			// Always re-envelope: a Contains skip would let attackers
			// forge envelope tags in tool output to slip content through
			// unenveloped. Stacked envelopes are bounded because
			// ApplyHistory defangs the lookalikes it wraps.
			wrapped = true
			out := s.envelope(content, tier, preamblePending)
			// Flip on first invocation: exactly one rule per request,
			// covering multi-part messages too.
			preamblePending = false
			return out
		}
		WalkMessageText(msg, redact)
		if wrapped {
			modified = true
			if s.tiersEnabled() {
				s.metrics.RecordSpotlightTier(tier, sourceKindOf(source))
			}
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

// tiersEnabled reports whether risk-tiered marking is configured on.
func (s *SpotlightInterceptor) tiersEnabled() bool {
	rt := s.global.RiskTiers
	return rt != nil && rt.Enabled != nil && *rt.Enabled
}

// envelope wraps one untrusted surface per the resolved tier: low keeps
// the plain delimiters envelope (historical behavior), high interleaves
// datamarking before the delimiters. Both carry the one-time preamble.
func (s *SpotlightInterceptor) envelope(content string, tier string, includePreamble bool) string {
	if tier == SpotlightTierHigh {
		return spotlightWithPreamble(SpotlightDatamarking(content), includePreamble)
	}
	return spotlightWithPreamble(content, includePreamble)
}

// resolveTier computes the envelope tier for one tool message: the
// deterministic heuristics force high (markers) or low (plain local
// output); the ambiguous band goes to the advisory judgment when wired
// and the per-request budget allows, failing closed to high. Returns
// the tier and the provenance source.
func (s *SpotlightInterceptor) resolveTier(ctx context.Context, agentName string, msg map[string]any, judgeBudget *int) (tier, source string) {
	source = "tool-history"
	if name, ok := msg["name"].(string); ok && name != "" {
		source = "tool:" + sanitizeSource(name)
	}

	if !s.tiersEnabled() {
		return SpotlightTierLow, source
	}
	rt := s.global.RiskTiers
	text := collectMessageText(msg)
	if strings.TrimSpace(text) == "" {
		// No text surfaces: nothing to envelope, never consult the judge.
		return SpotlightTierLow, source
	}
	t, marker := ClassifySpotlightTier(text, source, rt.SizeThresholdBytes)
	switch t {
	case SpotlightTierHigh:
		s.logTier(agentName, source, t, marker)
		return SpotlightTierHigh, source
	case SpotlightTierLow:
		return SpotlightTierLow, source
	}
	// Ambiguous band: adjudicate when wired and the budget allows,
	// fail closed to high otherwise.
	if s.tierJudge == nil || *judgeBudget <= 0 {
		return SpotlightTierHigh, source
	}
	*judgeBudget--
	input, innerTruncated := ComposeSpotlightTierInput(source, len(text), text, s.tierMaxBytes(rt))
	res := s.tierJudge.Adjudicate(ctx, agentName, input)
	res.Truncated = res.Truncated || innerTruncated
	return ResolveSpotlightTier(res), source
}

// logTier emits the debug forensics line for a marker-forced tier.
func (s *SpotlightInterceptor) logTier(agentName, source, tier, marker string) {
	if marker == "" {
		return
	}
	s.logger.Debug("spotlight tier forced by marker",
		"agent", agentName, "source", source, "tier", tier, "marker", marker)
}

// tierMaxBytes resolves the effective judgment excerpt budget: the
// configured value when set, else the judge's contract default.
func (s *SpotlightInterceptor) tierMaxBytes(rt *config.SpotlightRiskTiersConfig) int {
	if rt.MaxBytes > 0 {
		return rt.MaxBytes
	}
	return s.tierJudge.MaxBytes()
}

// collectMessageText concatenates every text surface of a message.
func collectMessageText(msg map[string]any) string {
	var b strings.Builder
	WalkMessageText(msg, func(surface string) string {
		b.WriteString(surface)
		b.WriteByte('\n')
		return surface
	})
	return b.String()
}

// sourceKindOf collapses a provenance source into the metric label kind.
func sourceKindOf(source string) string {
	if spotlightSourceIsWeb(source) {
		return "web"
	}
	return "local"
}
