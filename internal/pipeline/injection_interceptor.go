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

// Strict implements StrictInterceptor: detection is a security surface —
// an operational failure must not forward unscanned content. (Tier-2
// escalation failures intentionally fall back to the deterministic
// tier-1 verdict instead of erroring, so this only fires on unexpected
// internal faults.)
func (i *InjectionInterceptor) Strict() bool { return true }

// deterministicSecurity marks tier-1 injection detection for the MCP loop's
// rescan subset (which runs it with InterceptRequest.DisableEscalation).
func (i *InjectionInterceptor) deterministicSecurity() {}

// resolveSettings applies per-agent overrides on top of the global config
// for the canonical agent identity: the proxy-resolved Agent config when
// present (avoids re-deriving the agent from the wire), otherwise the
// name-keyed snapshot looked up via AgentNameFor. Unknown agents resolve
// to the global settings.
func (i *InjectionInterceptor) resolveSettings(req *InterceptRequest) (enabled, strict bool) {
	enabled = i.global.Enabled != nil && *i.global.Enabled
	strict = i.global.Strict != nil && *i.global.Strict
	if req.Agent != nil && req.Agent.Injection != nil {
		if req.Agent.Injection.Enabled != nil {
			enabled = *req.Agent.Injection.Enabled
		}
		if req.Agent.Injection.Strict != nil {
			strict = *req.Agent.Injection.Strict
		}
		return enabled, strict
	}
	agentName := AgentNameFor(req)
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

// bandDecision resolves the two-tier banding for a detection count: act
// immediately at/above maxScore, skip (pass untouched) below minScore, and
// escalate in the ambiguous band between them. When the request disables
// escalation (e.g. the MCP loop's deterministic-only rescan), only the
// escalation hop is suppressed — the ambiguous band falls back to the
// deterministic act verdict, while the below-min pass band still passes.
// skip=true short-circuits Process.
func (i *InjectionInterceptor) bandDecision(req *InterceptRequest, detections int) (act, skip bool) {
	if i.escalator == nil {
		return true, false
	}
	switch {
	case detections >= i.maxScore:
		return true, false
	case detections < i.minScore:
		return false, true
	case req.DisableEscalation:
		// Ambiguous band with the classifier suppressed: deterministic
		// tier-1 act verdict.
		return true, false
	default:
		return false, false
	}
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

	// Band decision. Nil escalator (escalation disabled in config): legacy
	// act-on-any bands. DisableEscalation (MCP rescan) collapses only the
	// ambiguous band to act — below-min still passes (see bandDecision).
	bandAct, skip := i.bandDecision(req, detections)
	if skip {
		return &InterceptResult{Payload: req.Payload, Skip: true}, nil
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
	agentName := AgentNameFor(req)
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
	original := content
	if stripped := stripInvisible(content); stripped != content {
		addHit(categoryHiddenText)
		content = stripped
	}

	raw := i.applyPatterns(content, nil, nil)
	content = raw.content
	hits += len(raw.matched) + raw.encoded
	categories = mergeCategories(categories, raw.categories)

	// NENYA-147: variant shapes (ANSI escapes, C0 controls, extended
	// invisible/tag characters, fullwidth homoglyphs, non-ASCII space
	// separators) can split a keyword so the raw pass misses it. Run the pass
	// over the normalized candidates as well and union the hits — the pass is
	// not gated on the raw result, because any hit on the same surface (hidden
	// text, an encoded blob, or a plain decoy phrase) would otherwise suppress
	// it and let an obfuscated instruction through in sanitize mode. Adoption is
	// still conditioned on the candidate revealing a *new* pattern, so benign
	// content is never rewritten by normalization alone, and a pattern the raw
	// pass already counted is never double-counted.
	joined, spaced := normalizeForMatching(content)
	candidates := []string{joined, spaced}
	if original != content {
		// The invisible-character strip ran: a separator hidden by one of
		// those characters is only restored by normalizing the original
		// surface (the strip removed it without leaving a separator).
		_, originalSpaced := normalizeForMatching(original)
		candidates = append(candidates, originalSpaced)
	}
	for _, candidate := range candidates {
		if candidate == content {
			continue
		}
		variant := i.applyPatterns(candidate, raw.matched, raw.encodedTokens)
		if len(variant.matched) == 0 && variant.content == candidate {
			// Nothing matched in the candidate at all: leave the surface as it
			// is (normalization alone never rewrites content).
			continue
		}
		content = variant.content
		hits += len(variant.matched) + variant.encoded
		categories = mergeCategories(categories, variant.categories)
		break
	}

	return content, hits, categories
}

// patternPass is the outcome of one pattern/encoded pass over a surface copy:
// the sanitized copy, the pattern indices counted by this pass, the encoded
// hits, and their categories.
type patternPass struct {
	content       string
	matched       map[int]bool
	encoded       int
	encodedTokens map[string]bool
	categories    map[injectionCategory]int
}

// applyPatterns runs the pattern pass and the encoded-blob scan over one surface
// copy. A pattern whose index is in seen still has its spans neutralized but is
// not counted again, so a variant pass over a normalized copy cannot
// double-count a match the raw pass already recorded (NENYA-147).
func (i *InjectionInterceptor) applyPatterns(content string, seen map[int]bool, seenEncoded map[string]bool) patternPass {
	pass := patternPass{content: content}
	for idx, pattern := range i.patterns {
		// Match against the progressively rewritten copy (a pattern whose span
		// an earlier pattern already neutralized must not count twice).
		if !pattern.re.MatchString(pass.content) {
			continue
		}
		pass.content = pattern.re.ReplaceAllString(pass.content, sanitizeMarker)
		if seen[idx] {
			continue
		}
		if pass.matched == nil {
			pass.matched = make(map[int]bool)
		}
		pass.matched[idx] = true
		pass.addCategory(pattern.category, 1)
	}
	found, tokens := scanEncodedTokens(pass.content)
	if found == 0 {
		return pass
	}
	for _, token := range tokens {
		pass.content = strings.ReplaceAll(pass.content, token, sanitizeMarker)
		if seenEncoded[token] {
			continue
		}
		if pass.encodedTokens == nil {
			pass.encodedTokens = make(map[string]bool)
		}
		pass.encodedTokens[token] = true
		pass.encoded++
	}
	if pass.encoded > 0 {
		pass.addCategory(categoryEncoded, pass.encoded)
	}
	return pass
}

// addCategory records n hits of one category on the pass.
func (p *patternPass) addCategory(category injectionCategory, n int) {
	if p.categories == nil {
		p.categories = make(map[injectionCategory]int)
	}
	p.categories[category] += n
}

// mergeCategories folds a second category tally into the first.
func mergeCategories(into, from map[injectionCategory]int) map[injectionCategory]int {
	if len(from) == 0 {
		return into
	}
	if into == nil {
		into = make(map[injectionCategory]int, len(from))
	}
	for category, count := range from {
		into[category] += count
	}
	return into
}

// recordMetrics emits one detection counter per category.
func (i *InjectionInterceptor) recordMetrics(action string, categoryHits map[injectionCategory]int) {
	for category, n := range categoryHits {
		i.metrics.RecordInjectionDetection(action, string(category), n)
	}
}
