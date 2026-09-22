package pipeline

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
)

// ErrInjectionDetected is the sentinel wrapped by strict-mode injection
// rejections; callers can match it with errors.Is.
var ErrInjectionDetected = errors.New("injection detected")

// InjectionInterceptor performs deterministic prompt-injection detection
// on message content (all roles, string and array forms, plus tool-call
// argument strings via WalkMessageText). Priority: 15 — after pattern
// redaction, before entropy. Default action is warn+sanitize (neutralize
// the matched span, record metrics); per-agent strict overrides reject
// the request with a structured 403 error_kind=injection_detected.
//
// Two-tier scoring: with governance.injection.escalation enabled,
// detection counts in the ambiguous band [min_score, max_score) go to an
// advisory LLM classifier through the engine chain before the tier-1
// verdict applies; counts at or above max_score act immediately, counts
// below min_score pass. Every classifier failure falls back to the
// deterministic verdict — the LLM layer can never weaken tier-1.
//
// Accepted limitations: per-surface scanning cannot catch payloads split
// across adjacent content-array parts; the sanitize marker is a
// spoofable literal (detection counters remain the source of truth).
type InjectionInterceptor struct {
	name            string
	priority        int
	global          config.InjectionConfig
	agentEnabled    map[string]*bool
	agentStrict     map[string]*bool
	patterns        []injectionPattern
	ignoreRe        []*regexp.Regexp
	escalator       *InjectionEscalator
	minScore        int
	maxScore        int
	perRequestLimit int
	metrics         *infra.Metrics
}

// compileInjectionDetectors compiles the built-in detector set plus the
// configured extra/ignore patterns. Shared by interceptor construction
// and ValidateInjectionPatterns so the reload pre-check exercises
// exactly the pattern-compilation failure mode it documents.
func compileInjectionDetectors(cfg *config.InjectionConfig) ([]injectionPattern, []*regexp.Regexp, error) {
	if cfg == nil {
		cfg = &config.InjectionConfig{}
	}
	patterns, err := builtInInjectionPatterns()
	if err != nil {
		return nil, nil, fmt.Errorf("built-in injection patterns: %w", err)
	}
	for _, source := range cfg.ExtraPatterns {
		compiled, err := compileInjectionPatterns([]patternSource{{categoryCustom, source}})
		if err != nil {
			return nil, nil, fmt.Errorf("injection extra_patterns: %w", err)
		}
		patterns = append(patterns, compiled...)
	}
	ignoreRe := make([]*regexp.Regexp, 0, len(cfg.IgnorePatterns))
	for _, source := range cfg.IgnorePatterns {
		re, err := regexp.Compile(source)
		if err != nil {
			return nil, nil, fmt.Errorf("injection ignore_patterns: %w", err)
		}
		ignoreRe = append(ignoreRe, re)
	}
	return patterns, ignoreRe, nil
}

// NewInjectionInterceptor compiles the built-in detector set plus any
// configured extra/ignore patterns, snapshots per-agent overrides
// (enabled/strict only; per-agent extra/ignore/escalation fields are
// rejected by config validation), and builds the tier-2 classifier when
// escalation is enabled with resolved engine targets. Returns an error
// when a configured pattern does not compile or the escalation engine
// cannot be constructed (fail-closed). The metrics receiver is nil-safe.
func NewInjectionInterceptor(cfg *config.InjectionConfig, agents map[string]config.AgentConfig, metrics *infra.Metrics, deps *InjectionEscalationDeps) (*InjectionInterceptor, error) {
	if cfg == nil {
		cfg = &config.InjectionConfig{}
	}
	patterns, ignoreRe, err := compileInjectionDetectors(cfg)
	if err != nil {
		return nil, err
	}

	agentEnabled := make(map[string]*bool, len(agents))
	agentStrict := make(map[string]*bool, len(agents))
	for name, agent := range agents {
		if agent.Injection == nil {
			continue
		}
		agentEnabled[name] = agent.Injection.Enabled
		agentStrict[name] = agent.Injection.Strict
	}

	escalator, minScore, maxScore, perRequestLimit, err := buildEscalator(cfg, deps)
	if err != nil {
		return nil, err
	}

	return &InjectionInterceptor{
		name:            "injection",
		priority:        15,
		global:          *cfg,
		agentEnabled:    agentEnabled,
		agentStrict:     agentStrict,
		patterns:        patterns,
		ignoreRe:        ignoreRe,
		escalator:       escalator,
		minScore:        minScore,
		maxScore:        maxScore,
		perRequestLimit: perRequestLimit,
		metrics:         metrics,
	}, nil
}

// buildEscalator constructs the tier-2 classifier when escalation is
// enabled; a disabled or unresolvable escalation degrades as follows:
//   - escalation absent/disabled: nil escalator, legacy act-on-any bands
//   - enabled but engine missing/unresolved, or deps missing: error
//     (fail-closed — silent no-op would break the startup contract)
//
// The configured band values are honored as-is: validated configs
// (ApplyDefaults + validateInjectionEscalation) guarantee min >= 1 and
// max > min. Direct-built configs that skip validation get predictable
// semantics: maxScore <= 0 means act-on-any (detections >= 0 always
// holds), and minScore > maxScore simply narrows the escalate window to
// empty (act at/above max, pass below min).
func buildEscalator(cfg *config.InjectionConfig, deps *InjectionEscalationDeps) (*InjectionEscalator, int, int, int, error) {
	esc := cfg.GetEscalation()
	if esc == nil {
		return nil, 1, 0, 1, nil
	}
	if esc.Engine == nil || len(esc.Engine.ResolvedTargets) == 0 {
		return nil, 1, 0, 1, errors.New("injection escalation enabled but engine missing or unresolved")
	}
	if deps == nil {
		return nil, 1, 0, 1, errors.New("injection escalation enabled but no engine deps provided")
	}
	escalator, err := NewInjectionEscalator(esc.Engine, *deps, esc.MaxBytes)
	if err != nil {
		return nil, 1, 0, 1, err
	}
	return escalator, esc.MinScore, esc.MaxScore, esc.PerRequestLimit, nil
}

// ValidateInjectionPatterns compiles the built-in detector set plus the
// configured extra/ignore patterns without constructing an interceptor.
// Config reload flows call this BEFORE swapping the live gateway to
// prove the one unprechecked chain-build failure mode (pattern
// compilation; escalation construction is pre-checked by
// validateInjectionEscalation and resolveEngineRefs during Load), keeping
// reload abort paths from having to unwind a closed gateway. Pre-validation is complete only because per-agent pattern
// fields are rejected earlier by config.validateInjectionConfig.
func ValidateInjectionPatterns(cfg *config.InjectionConfig) error {
	_, _, err := compileInjectionDetectors(cfg)
	return err
}

func (i *InjectionInterceptor) Name() string  { return i.name }
func (i *InjectionInterceptor) Priority() int { return i.priority }

// resolveSettings applies per-agent overrides on top of the global config
// for the agent named in the payload's model field (agent scoping uses the
// top-level model field). Missing or non-string model fields resolve to
// the global settings.
func (i *InjectionInterceptor) resolveSettings(req *InterceptRequest) (enabled, strict bool) {
	enabled = i.global.Enabled != nil && *i.global.Enabled
	strict = i.global.Strict != nil && *i.global.Strict
	agentName, _ := req.Payload["model"].(string)
	if override, ok := i.agentEnabled[agentName]; ok && override != nil {
		enabled = *override
	}
	if override, ok := i.agentStrict[agentName]; ok && override != nil {
		strict = *override
	}
	return enabled, strict
}

func (i *InjectionInterceptor) CanHandle(ctx context.Context, req *InterceptRequest) bool {
	if ctx.Err() != nil {
		return false
	}
	enabled, _ := i.resolveSettings(req)
	return enabled && len(req.Messages) > 0
}

// Process runs detection in two phases: a read-only pass decides sanitize
// versus reject (strict rejections abort before any payload mutation,
// preserving forensics), then the rewrite pass applies neutralization.
// With escalation enabled, ambiguous-band requests consult the tier-2
// classifier between the phases: benign verdicts clear their surfaces
// (no sanitization, no strict rejection), while injection verdicts,
// classifier errors, budget exhaustion, and summarized traffic all fall
// back to the deterministic tier-1 verdict.
func (i *InjectionInterceptor) Process(ctx context.Context, req *InterceptRequest) (*InterceptResult, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	enabled, strict := i.resolveSettings(req)
	if !enabled {
		// Defensive: direct Process calls bypassing CanHandle stay inert.
		return &InterceptResult{Payload: req.Payload, Skip: true}, nil
	}

	// Phase 1: read-only detection totals, collecting unique hit
	// surfaces for potential escalation (deduped: identical content in
	// two messages is one classification target and one budget unit).
	seen := make(map[string]bool)
	var hitSurfaces []string
	detections := 0
	categoryHits := map[injectionCategory]int{}
	count := func(content string) string {
		_, hits, categories := i.scan(content)
		if hits > 0 && !seen[content] {
			seen[content] = true
			hitSurfaces = append(hitSurfaces, content)
		}
		detections += hits
		for category, n := range categories {
			categoryHits[category] += n
		}
		return content
	}
	for _, msg := range req.Messages {
		WalkMessageText(msg, count)
	}

	if detections == 0 {
		return &InterceptResult{Payload: req.Payload, Skip: true}, nil
	}

	// Band decision. Legacy single-tier mode (escalator nil): any
	// detection acts.
	bandAct := true
	if i.escalator != nil {
		switch {
		case detections >= i.maxScore:
			bandAct = true
		case detections < i.minScore:
			return &InterceptResult{Payload: req.Payload, Skip: true}, nil
		default:
			bandAct = false
		}
	}

	if !bandAct {
		return i.processEscalated(ctx, req, strict, hitSurfaces, categoryHits)
	}

	if strict {
		i.recordMetrics("reject", categoryHits)
		return nil, &RejectError{
			Err:     fmt.Errorf("%w: %d pattern match(es) in request messages (strict mode)", ErrInjectionDetected, detections),
			Kind:    infra.ErrorKindInjection,
			Message: "request rejected by injection policy",
		}
	}

	// Phase 2: neutralization rewrite. Re-scanning doubles regex cost on
	// sanitized requests — accepted in exchange for pristine payloads on
	// strict rejection (detections were already counted in phase 1).
	shrunk := false
	for _, msg := range req.Messages {
		WalkMessageText(msg, func(content string) string {
			clean, _, _ := i.scan(content)
			if len(clean) < len(content) {
				shrunk = true
			}
			return clean
		})
	}

	i.recordMetrics("sanitize", categoryHits)
	// WalkMessageText mutates the shared message maps in place; the
	// original payload["messages"] slice ([]interface{}) references the
	// same maps, so no slice swap is needed (and swapping would corrupt
	// downstream []interface{} type assertions).
	return &InterceptResult{
		Payload:   req.Payload,
		Truncated: shrunk,
		Reason:    "injection_sanitized",
	}, nil
}

// processEscalated resolves an ambiguous-band request through the tier-2
// classifier and applies the verdicts: surfaces cleared as benign are
// exempt from the tier-1 action; injection verdicts, classifier errors,
// truncated-excerpt benigns (inconclusive), budget exhaustion, and
// summarized traffic all fall back to tier-1.
func (i *InjectionInterceptor) processEscalated(ctx context.Context, req *InterceptRequest, strict bool, hitSurfaces []string, categoryHits map[injectionCategory]int) (*InterceptResult, error) {
	agentName, _ := req.Payload["model"].(string)
	benign := make(map[string]bool, len(hitSurfaces))
	confirmed := false
	fallbackReason := ""

	if i.summarizedTraffic(req) {
		// Traffic already headed for engine summarization gains nothing
		// from a second classifier hop; apply the deterministic verdict.
		confirmed = true
		fallbackReason = "summarized traffic"
	} else {
		confirmed, benign, fallbackReason = i.classifySurfaces(ctx, agentName, hitSurfaces)
	}

	allBenign := !confirmed && len(benign) == len(hitSurfaces)
	if allBenign {
		i.metrics.RecordInjectionDetection("cleared", "escalated", len(hitSurfaces))
		return &InterceptResult{Payload: req.Payload, Skip: true}, nil
	}

	if strict {
		i.recordMetrics("reject", categoryHits)
		detail := "classifier confirmed"
		if fallbackReason != "" {
			detail = "fallback: " + fallbackReason
		}
		return nil, &RejectError{
			Err:     fmt.Errorf("%w: %d hit surface(s) in request messages (strict mode, %s)", ErrInjectionDetected, len(hitSurfaces), detail),
			Kind:    infra.ErrorKindInjection,
			Message: "request rejected by injection policy",
		}
	}

	shrunk := false
	for _, msg := range req.Messages {
		WalkMessageText(msg, func(content string) string {
			if benign[content] {
				return content
			}
			clean, _, _ := i.scan(content)
			if len(clean) < len(content) {
				shrunk = true
			}
			return clean
		})
	}

	i.recordMetrics("sanitize", categoryHits)
	return &InterceptResult{
		Payload:   req.Payload,
		Truncated: shrunk,
		Reason:    "injection_sanitized",
	}, nil
}

// classifySurfaces runs the tier-2 classifier over the hit surfaces
// within the per-request budget, returning whether the tier-1 verdict is
// confirmed, the set of surfaces cleared as benign, and the fallback
// reason for the audit trail. Truncated-excerpt benign verdicts are
// recorded as "inconclusive" (the unexamined tail could carry the
// payload) and fall back to tier-1.
func (i *InjectionInterceptor) classifySurfaces(ctx context.Context, agentName string, hitSurfaces []string) (bool, map[string]bool, string) {
	benign := make(map[string]bool, len(hitSurfaces))
	budget := i.perRequestLimit
	for _, content := range hitSurfaces {
		if budget <= 0 {
			// Unclassified surfaces keep the tier-1 verdict.
			return true, benign, "escalation budget exhausted"
		}
		budget--
		verdict, truncated, duration := i.escalator.classify(ctx, agentName, content)
		recorded := verdict
		if verdict == verdictBenign && truncated {
			// Keep the observable verdict honest without weakening the
			// deterministic floor: inconclusive still falls back to tier-1.
			recorded = verdictInconclusive
		}
		i.metrics.RecordInjectionEscalation(recorded, duration)
		switch {
		case verdict == verdictBenign && !truncated:
			benign[content] = true
		case verdict == verdictBenign:
			return true, benign, "truncated excerpt inconclusive"
		case verdict == verdictInjection:
			return true, benign, ""
		default: // classifier error: deterministic floor applies
			return true, benign, "classifier unavailable"
		}
	}
	return false, benign, ""
}

// summarizedTraffic reports whether the request is already headed for
// engine summarization (token count at or above the soft limit), in
// which case tier-2 escalation adds pure latency. Accepted
// approximation: req.TokenCount is the pre-chain snapshot — traffic that
// later phases will push over the limit may still be escalated (extra
// latency only, never a missed defense).
func (i *InjectionInterceptor) summarizedTraffic(req *InterceptRequest) bool {
	return req.SoftLimit > 0 && req.TokenCount >= req.SoftLimit
}

// scan runs every detector against one content surface and returns the
// sanitized text, the total hit count, and per-category hit counts (nil
// when nothing matched). Ignore patterns suppress the whole surface:
// an allowlisted phrase neutralizes all scanning on that content, so
// operators should keep ignore_patterns narrow.
func (i *InjectionInterceptor) scan(content string) (string, int, map[injectionCategory]int) {
	for _, ignoreRe := range i.ignoreRe {
		if ignoreRe.MatchString(content) {
			return content, 0, nil
		}
	}

	var categories map[injectionCategory]int
	hits := 0
	addHit := func(category injectionCategory) {
		hits++
		if categories == nil {
			categories = make(map[injectionCategory]int)
		}
		categories[category]++
	}

	// Invisible formatting characters are stripped outright: they exist to
	// hide instructions, and removing them preserves surrounding prose.
	if stripped := stripInvisible(content); stripped != content {
		addHit(categoryHiddenText)
		content = stripped
	}

	for _, pattern := range i.patterns {
		if pattern.re.MatchString(content) {
			addHit(pattern.category)
			content = pattern.re.ReplaceAllString(content, sanitizeMarker)
		}
	}

	if encodedHits, tokens := scanEncodedTokens(content); encodedHits > 0 {
		hits += encodedHits
		if categories == nil {
			categories = make(map[injectionCategory]int)
		}
		categories[categoryEncoded] += encodedHits
		for _, token := range tokens {
			content = strings.ReplaceAll(content, token, sanitizeMarker)
		}
	}

	return content, hits, categories
}

// recordMetrics emits one detection counter per category.
func (i *InjectionInterceptor) recordMetrics(action string, categoryHits map[injectionCategory]int) {
	for category, n := range categoryHits {
		i.metrics.RecordInjectionDetection(action, string(category), n)
	}
}
