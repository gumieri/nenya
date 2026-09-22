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
// Accepted limitations: per-surface scanning cannot catch payloads split
// across adjacent content-array parts; the sanitize marker is a
// spoofable literal (detection counters remain the source of truth).
type InjectionInterceptor struct {
	name         string
	priority     int
	global       config.InjectionConfig
	agentEnabled map[string]*bool
	agentStrict  map[string]*bool
	patterns     []injectionPattern
	ignoreRe     []*regexp.Regexp
	metrics      *infra.Metrics
}

// NewInjectionInterceptor compiles the built-in detector set plus any
// configured extra/ignore patterns and snapshots per-agent overrides
// (enabled/strict only; per-agent extra/ignore patterns are rejected by
// config validation). Returns an error when a configured pattern does not
// compile. The metrics receiver is nil-safe.
func NewInjectionInterceptor(cfg *config.InjectionConfig, agents map[string]config.AgentConfig, metrics *infra.Metrics) (*InjectionInterceptor, error) {
	if cfg == nil {
		cfg = &config.InjectionConfig{}
	}
	patterns, err := builtInInjectionPatterns()
	if err != nil {
		return nil, fmt.Errorf("built-in injection patterns: %w", err)
	}
	for _, source := range cfg.ExtraPatterns {
		compiled, err := compileInjectionPatterns([]patternSource{{categoryCustom, source}})
		if err != nil {
			return nil, fmt.Errorf("injection extra_patterns: %w", err)
		}
		patterns = append(patterns, compiled...)
	}
	ignoreRe := make([]*regexp.Regexp, 0, len(cfg.IgnorePatterns))
	for _, source := range cfg.IgnorePatterns {
		re, err := regexp.Compile(source)
		if err != nil {
			return nil, fmt.Errorf("injection ignore_patterns: %w", err)
		}
		ignoreRe = append(ignoreRe, re)
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

	return &InjectionInterceptor{
		name:         "injection",
		priority:     15,
		global:       *cfg,
		agentEnabled: agentEnabled,
		agentStrict:  agentStrict,
		patterns:     patterns,
		ignoreRe:     ignoreRe,
		metrics:      metrics,
	}, nil
}

// ValidateInjectionPatterns compiles the built-in detector set plus the
// configured extra/ignore patterns without constructing an interceptor.
// Config reload flows call this BEFORE swapping the live gateway: the only
// buildInterceptorChain failure mode is pattern compilation, and proving
// it beforehand keeps reload abort paths from having to unwind a closed
// gateway. Pre-validation is complete only because per-agent pattern
// fields are rejected earlier by config.validateInjectionConfig.
func ValidateInjectionPatterns(cfg *config.InjectionConfig) error {
	_, err := NewInjectionInterceptor(cfg, nil, nil)
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
func (i *InjectionInterceptor) Process(ctx context.Context, req *InterceptRequest) (*InterceptResult, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	enabled, strict := i.resolveSettings(req)
	if !enabled {
		// Defensive: direct Process calls bypassing CanHandle stay inert.
		return &InterceptResult{Payload: req.Payload, Skip: true}, nil
	}

	// Phase 1: read-only detection totals.
	detections := 0
	categoryHits := map[injectionCategory]int{}
	count := func(content string) string {
		_, hits, categories := i.scan(content)
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
