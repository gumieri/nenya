package routing

import (
	"sort"
	"strings"
)

// Request normalization for coding-agent harness payloads (NENYA-186). Two
// classes of harness quirk are neutralized here, loudly:
//
//  1. Duplicated parameters after a format conversion. A harness may send the
//     newer spelling (max_completion_tokens) while the gateway injects the
//     canonical one (max_tokens), or a conversion may carry both spellings of
//     one logical knob (stop / stop_sequences). Leaving both in place is
//     ambiguous — upstreams pick one, or reject the body. The normalizer
//     resolves the conflict to a single canonical field and records it.
//
//  2. Unknown top-level fields. Every request field is either consumed by a
//     format path or explicitly reported, so a silently dropped envelope
//     (the LiteLLM "additional_tools" class) is visible instead of costing a
//     capability with no trace.
//
// Both record structured telemetry (closed-label counters + a Warn log naming
// the fields), never silence.

// duplicateParamRule pairs two spellings of one logical parameter. The
// canonical field is the one the target formats consume (and the gateway
// manages); the alias is the newer spelling a harness may send instead.
type duplicateParamRule struct {
	canonical string
	alias     string
	label     string
}

var duplicateParamRules = []duplicateParamRule{
	{canonical: "max_tokens", alias: "max_completion_tokens", label: "max_tokens"},
	{canonical: "stop", alias: "stop_sequences", label: "stop"},
}

// normalizeDuplicateParams resolves conflicting duplicate parameters in place:
// the canonical field survives with the conservative value (a token cap keeps
// the SMALLER of the two, so neither the client's tighter budget nor the
// gateway's model clamp can be exceeded), and the conflict is reported.
// Duplicates whose values agree — or whose alias is JSON null — collapse
// without telemetry (no ambiguity to report). The function is idempotent: a
// retry/failover re-transform of the same payload finds no alias left, so the
// conflict is reported once per request even though the dispatch funnel runs
// per attempt.
func normalizeDuplicateParams(deps TransformDeps, payload map[string]interface{}) {
	for _, rule := range duplicateParamRules {
		primary, hasPrimary := payload[rule.canonical]
		alias, hasAlias := payload[rule.alias]
		if !hasPrimary || !hasAlias {
			continue
		}
		if alias == nil {
			delete(payload, rule.alias)
			continue
		}
		if primary == nil {
			payload[rule.canonical] = alias
			delete(payload, rule.alias)
			continue
		}
		if equalParamValues(primary, alias) {
			delete(payload, rule.alias)
			continue
		}
		if pv, ok := numericValue(primary); ok {
			if av, ok := numericValue(alias); ok && av < pv {
				// Keep the smaller cap: neither the client's tighter
				// budget nor the gateway's model clamp is exceeded.
				payload[rule.canonical] = av
			}
		}
		delete(payload, rule.alias)
		deps.Metrics.RecordDuplicateParam(rule.label)
		if deps.Logger != nil {
			deps.Logger.Warn("resolved conflicting duplicate request parameters",
				"param", rule.label, "alias", rule.alias,
				"kept_value", payload[rule.canonical])
		}
	}
}

// numericValue coerces the numeric shapes a payload can carry in memory —
// JSON decodes produce float64, while gateway-injected and programmatically
// built values may be int or int64 — so comparisons do not silently no-op on
// a type mismatch (NENYA-186 review).
func numericValue(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// equalParamValues reports whether two parameter values are equal in the
// shapes a payload carries (numbers in any numeric type, string, bool, and
// slices thereof).
func equalParamValues(a, b interface{}) bool {
	if an, ok := numericValue(a); ok {
		bn, ok := numericValue(b)
		return ok && an == bn
	}
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case []interface{}:
		bv, ok := b.([]interface{})
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !equalParamValues(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// knownRequestFields is the set of top-level fields some format path consumes
// (or that the gateway itself injects): OpenAI chat-completions, Anthropic
// Messages, the gateway's own knobs, and the passthrough-shaped extras. A
// field outside this set is reported, never silently forwarded-and-forgotten.
var knownRequestFields = map[string]struct{}{
	// OpenAI chat completions + gateway knobs.
	"model": {}, "messages": {}, "stream": {}, "stream_options": {},
	"max_tokens": {}, "max_completion_tokens": {}, "temperature": {}, "top_p": {},
	"top_k": {}, "n": {}, "stop": {}, "presence_penalty": {}, "frequency_penalty": {},
	"logit_bias": {}, "logprobs": {}, "top_logprobs": {}, "user": {},
	"tools": {}, "tool_choice": {}, "parallel_tool_calls": {}, "response_format": {},
	"seed": {}, "service_tier": {}, "metadata": {}, "store": {},
	"reasoning_effort": {}, "reasoning": {}, "reasoning_content": {},
	"thinking": {}, "modalities": {}, "prediction": {}, "audio": {},
	"prompt_cache_key": {}, "cache_control": {}, "session_id": {},
	// Anthropic Messages.
	"system": {}, "stop_sequences": {}, "type": {},
	// Gateway-injected internals that may ride the payload.
	"cache_salt": {}, "prompt_cache_options": {}, "extra_body": {},
}

// auditRequestFields reports top-level fields no format path consumes. The
// counter label is the resolved format (closed set); the field names go to the
// structured log, capped so a hostile body cannot flood the log line.
func auditRequestFields(deps TransformDeps, payload map[string]interface{}, format string) {
	if len(payload) == 0 {
		return
	}
	if format == "" {
		// The OpenAI wire is the default: registry/discovery leave Format
		// empty for OpenAI-family models, and the audit exists for exactly
		// that traffic (NENYA-186 review).
		format = "openai"
	}
	var unknown []string
	for field := range payload {
		if _, known := knownRequestFields[field]; known {
			continue
		}
		unknown = append(unknown, field)
	}
	if len(unknown) == 0 {
		return
	}
	sort.Strings(unknown)
	const maxLogged = 16
	logged := unknown
	if len(logged) > maxLogged {
		logged = logged[:maxLogged]
	}
	for i, field := range logged {
		if len(field) > 64 {
			logged[i] = field[:64]
		}
	}
	deps.Metrics.RecordUnknownRequestField(format)
	if deps.Logger != nil {
		deps.Logger.Warn("request carries fields no format path consumes",
			"format", format, "count", len(unknown), "fields", strings.Join(logged, ","))
	}
}
