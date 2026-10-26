package routing

import (
	"math"
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
//     resolves the conflict to a single surviving field (the canonical
//     spelling by default, the spelling a target declares via param_compat's
//     token_field when one matches) and records it.
//
//  2. Unknown top-level fields. Every request field is either consumed by a
//     format path or explicitly reported, so a silently dropped envelope
//     (the LiteLLM "additional_tools" class) is visible instead of costing a
//     capability with no trace.
//
// Both record structured telemetry (closed-label counters + a Warn log naming
// the fields), never silence.

// duplicateParamRule pairs two spellings of one logical parameter.
type duplicateParamRule struct {
	canonical string
	alias     string
	label     string
}

var duplicateParamRules = []duplicateParamRule{
	{canonical: "max_tokens", alias: "max_completion_tokens", label: "max_tokens"},
	{canonical: "stop", alias: "stop_sequences", label: "stop"},
}

// normalizeTargetParams applies the target-aware half of the harness
// normalization (NENYA-179): the mutation is a function of the (client format,
// target executor) pair, not of the client format alone.
//
//   - The duplicate-spelling resolution emits the spelling the TARGET requires
//     (param_compat `token_field`, e.g. max_completion_tokens for OpenAI
//     reasoning/Codex executors) instead of always the canonical one, keeping
//     the SMALLER cap so neither the client's budget nor the gateway's model
//     clamp is exceeded.
//   - Parameters the target declares integer-typed (`int_typed_params`) are
//     coerced from the client's JSON-decoded float shape to an integer, with
//     the (client format, target, param) pair recorded — a strict-typed
//     executor must not be handed a float, and the coercion must never be
//     silent.
//
// Tolerant targets (no matching rule) keep the canonical spelling and the
// client's value shape untouched.
func normalizeTargetParams(deps TransformDeps, payload map[string]interface{}, rules []paramCompatRule, modelName, sourceFormat string) {
	rule := matchParamCompatRule(rules, modelName)
	tokenField := ""
	var intTyped []string
	if rule != nil {
		tokenField = rule.tokenField
		intTyped = rule.intTypedParams
	}
	normalizeDuplicateParams(deps, payload, tokenField)
	moveCapToTargetField(deps, payload, tokenField, modelName)
	// Coerce the EFFECTIVE token field: after a move the cap lives under the
	// target's spelling, so a rule listing only the other name would
	// otherwise leave the declared type unconverted (NENYA-179 review).
	coerceIntTypedParams(deps, payload, intTyped, tokenField, sourceFormat)
}

// moveCapToTargetField moves a token cap that arrived under the name the
// target does NOT require (NENYA-179 "expanded field mappings"): when the
// target declares max_completion_tokens and the cap sits under max_tokens
// alone, the value is moved so a client's cap is never dropped just because
// the executor rejects its spelling.
func moveCapToTargetField(deps TransformDeps, payload map[string]interface{}, tokenField, modelName string) {
	if tokenField == "" || tokenField == "max_tokens" {
		return
	}
	if _, hasTarget := payload[tokenField]; hasTarget {
		return
	}
	v, hasCanonical := payload["max_tokens"]
	if !hasCanonical {
		return
	}
	payload[tokenField] = v
	delete(payload, "max_tokens")
	if deps.Logger != nil {
		// The move is the one normalization with no counter of its own: the
		// client's cap keeps its value under the name the target requires,
		// and the field move is named in the log (NENYA-179 review).
		deps.Logger.Info("moved token cap to the target's required field",
			"from", "max_tokens", "to", tokenField, "model", modelName)
	}
}

// normalizeDuplicateParams resolves conflicting duplicate parameters in place.
// tokenField, when non-empty, is the spelling the target requires and becomes
// the surviving field (the canonical spelling is the default); a token cap
// keeps the SMALLER of the two values, so neither the client's tighter budget
// nor the gateway's model clamp can be exceeded. Agreeing duplicates and JSON
// null aliases collapse without telemetry; conflicts are counted and logged.
// The function is idempotent: a retry/failover re-transform of the same
// payload finds no alias left, so a conflict is reported once per request even
// though the dispatch funnel runs per attempt.
func normalizeDuplicateParams(deps TransformDeps, payload map[string]interface{}, tokenField string) {
	for _, rule := range duplicateParamRules {
		keep, drop := rule.canonical, rule.alias
		if rule.canonical == "max_tokens" && tokenField == rule.alias {
			// The target requires the alias spelling: the client's cap must
			// survive under the name this executor accepts (NENYA-179).
			keep, drop = rule.alias, rule.canonical
		}
		primary, hasPrimary := payload[keep]
		alias, hasAlias := payload[drop]
		if !hasPrimary || !hasAlias {
			continue
		}
		if alias == nil {
			delete(payload, drop)
			continue
		}
		if primary == nil {
			payload[keep] = alias
			delete(payload, drop)
			continue
		}
		if equalParamValues(primary, alias) {
			delete(payload, drop)
			continue
		}
		if pv, ok := numericValue(primary); ok {
			if av, ok := numericValue(alias); ok && av < pv {
				// Keep the smaller cap: neither the client's tighter
				// budget nor the gateway's model clamp is exceeded.
				payload[keep] = av
			}
		}
		delete(payload, drop)
		deps.Metrics.RecordDuplicateParam(rule.label)
		if deps.Logger != nil {
			deps.Logger.Warn("resolved conflicting duplicate request parameters",
				"param", rule.label, "kept", keep, "kept_value", payload[keep])
		}
	}
}

// coerceIntTypedParams rewrites integer-typed parameters the target declares
// from the client's float shape to an integer. Only integral values are
// coerced (a fractional cap is a client bug, not a value to round silently)
// and each coercion is recorded with the client format it came from.
func coerceIntTypedParams(deps TransformDeps, payload map[string]interface{}, params []string, tokenField, sourceFormat string) {
	for _, param := range params {
		if tokenField != "" && param == "max_tokens" && tokenField == "max_completion_tokens" {
			param = tokenField // the cap now lives under the target's spelling
		}
		v, ok := payload[param]
		if !ok {
			continue
		}
		f, ok := numericValue(v)
		if !ok {
			continue
		}
		if _, isInt := v.(int); isInt {
			continue // already integer-typed
		}
		if _, isInt64 := v.(int64); isInt64 {
			continue // already integer-typed
		}
		if f != float64(int64(f)) || f < 0 {
			continue // fractional or negative: leave the client's value alone
		}
		clamped := false
		if f > math.MaxInt32 {
			f = math.MaxInt32 // never overflow a 32-bit int conversion
			clamped = true
		}
		payload[param] = int(f)
		deps.Metrics.RecordParamCoercion(param, sourceFormat)
		if deps.Logger != nil {
			deps.Logger.Debug("coerced parameter to the target's integer type",
				"param", param, "client_format", sourceFormat, "value", int(f), "clamped", clamped)
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
//
// The audit reports the CLIENT's fields once per LOGICAL request, not once
// per failover attempt: deps.UnknownFieldsAudited (a shared bool owned by
// the request's retry loop) gates the re-fire (AUD-39 / NENYA-245). A nil
// pointer keeps the per-attempt semantics for callers without request scope.
func auditRequestFields(deps TransformDeps, payload map[string]interface{}, format string) {
	if deps.UnknownFieldsAudited != nil {
		if *deps.UnknownFieldsAudited {
			return
		}
		*deps.UnknownFieldsAudited = true
	}
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
