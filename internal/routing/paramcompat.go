package routing

import (
	"strings"

	"github.com/nenya/config"
)

// Reasoning effort levels, ordered weakest to strongest. Used by the
// param-compat clamp so "minimal" never reaches models that reject it.
var reasoningEffortOrder = [...]string{"minimal", "low", "medium", "high", "xhigh", "max"}

// paramCompatRule is the resolved form of config.ParamCompatRule with a
// lowercased prefix for matching.
type paramCompatRule struct {
	prefix                string
	dropParams            []string
	minReasoningEffort    string
	relaxForcedToolChoice bool
}

// builtInParamCompat encodes the known new-generation rejections
// (NENYA-32 evidence): Gemini 3 rejects sampling parameters and
// candidate_count; several models reject the "minimal" reasoning effort.
// Config rules take precedence over this table.
var builtInParamCompat = []paramCompatRule{
	{
		prefix:     "gemini-3",
		dropParams: []string{"temperature", "top_p", "top_k", "presence_penalty", "frequency_penalty", "candidate_count"},
	},
}

// resolveParamCompatRules merges config-declared rules (first) with the
// built-in table (last). First matching rule wins at lookup time, so
// operators can override built-in behavior per prefix.
func resolveParamCompatRules(cfg *config.GovernanceConfig) []paramCompatRule {
	if cfg == nil || len(cfg.ParamCompat) == 0 {
		return builtInParamCompat
	}
	rules := make([]paramCompatRule, 0, len(cfg.ParamCompat)+len(builtInParamCompat))
	for _, r := range cfg.ParamCompat {
		if r.ModelPrefix == "" {
			continue
		}
		rules = append(rules, paramCompatRule{
			prefix:                strings.ToLower(r.ModelPrefix),
			dropParams:            r.DropParams,
			minReasoningEffort:    strings.ToLower(r.MinReasoningEffort),
			relaxForcedToolChoice: r.RelaxForcedToolChoice,
		})
	}
	return append(rules, builtInParamCompat...)
}

// matchParamCompatRule returns the first rule whose prefix is a prefix of
// the model ID (segment-aware on "/" delimiters, mirroring discovery's
// capability matching). Returns nil when nothing matches.
func matchParamCompatRule(rules []paramCompatRule, model string) *paramCompatRule {
	if model == "" {
		return nil
	}
	lm := strings.ToLower(model)
	for i := range rules {
		r := &rules[i]
		for _, seg := range strings.Split(lm, "/") {
			if strings.HasPrefix(seg, r.prefix) {
				return r
			}
		}
	}
	return nil
}

// applyParamCompat sanitizes the payload against the resolved rule set:
// dropped parameters, reasoning-effort clamping, and forced tool_choice
// relaxation. Returns the names of the parameters it changed so callers
// can log and record metrics.
func applyParamCompat(rules []paramCompatRule, payload map[string]interface{}, model string) []string {
	rule := matchParamCompatRule(rules, model)
	if rule == nil {
		return nil
	}
	var applied []string
	for _, param := range rule.dropParams {
		if _, ok := payload[param]; ok {
			delete(payload, param)
			applied = append(applied, param)
		}
	}
	if rule.minReasoningEffort != "" {
		if clamped := clampReasoningEffort(payload, rule.minReasoningEffort); clamped {
			applied = append(applied, "reasoning_effort")
		}
	}
	if rule.relaxForcedToolChoice {
		if relaxed := relaxForcedToolChoice(payload); relaxed {
			applied = append(applied, "tool_choice")
		}
	}
	return applied
}

// clampReasoningEffort raises the payload's reasoning_effort to at least
// floor when present and weaker than the floor. Returns true when the
// value was rewritten.
func clampReasoningEffort(payload map[string]interface{}, floor string) bool {
	effort, ok := payload["reasoning_effort"].(string)
	if !ok || effort == "" {
		return false
	}
	floorIdx := effortIndex(floor)
	if floorIdx < 0 {
		return false
	}
	curIdx := effortIndex(strings.ToLower(effort))
	if curIdx < 0 || curIdx >= floorIdx {
		// Unknown values are left untouched: clamping an effort we do
		// not understand could rewrite a provider-specific token.
		return false
	}
	payload["reasoning_effort"] = floor
	return true
}

// relaxForcedToolChoice rewrites forced tool_choice variants ("required",
// {"type":"required"}, {"type":"function",...}) to "auto". Returns true
// when the value was rewritten.
func relaxForcedToolChoice(payload map[string]interface{}) bool {
	tc, ok := payload["tool_choice"]
	if !ok {
		return false
	}
	switch v := tc.(type) {
	case string:
		if v == "required" {
			payload["tool_choice"] = "auto"
			return true
		}
	case map[string]interface{}:
		typ, _ := v["type"].(string)
		if typ == "required" || typ == "function" {
			payload["tool_choice"] = "auto"
			return true
		}
	}
	return false
}

// effortIndex returns the position of effort in the ordered scale, or -1
// for unknown values (unknown values are never clamped).
func effortIndex(effort string) int {
	for i, e := range reasoningEffortOrder {
		if e == effort {
			return i
		}
	}
	return -1
}

// StripParams removes the named top-level parameters from the payload
// (reactive safety net for upstream parameter rejections).
func StripParams(payload map[string]interface{}, params []string) {
	for _, p := range params {
		delete(payload, p)
	}
}
