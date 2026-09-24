package mcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"
)

// DefaultMaxArgBytes is the default tool-argument size cap (1 MiB),
// enforced before schema validation and dispatch.
const DefaultMaxArgBytes = 1 << 20

// maxSchemaDepth caps JSON-schema recursion so a pathological (deeply
// nested or cyclic-by-construction) tool schema cannot exhaust the
// stack during validation.
const maxSchemaDepth = 16

// URL policy modes for the MCP tool-argument guard.
const (
	// URLPolicyDenyPrivate rejects http(s) argument URLs whose host is
	// private, loopback, or link-local (default).
	URLPolicyDenyPrivate = "deny_private"
	// URLPolicyLog records private-URL usage without rejecting.
	URLPolicyLog = "log"
	// URLPolicyOff disables URL checks entirely.
	URLPolicyOff = "off"
)

// ArgViolation describes one rejected or flagged argument.
type ArgViolation struct {
	// Path is the JSON-ish location inside the arguments (e.g. "url",
	// "items[2].host").
	Path string
	// Reason is a short human-readable explanation for the model.
	Reason string
}

// ArgGuardKnobs is the config-side knob surface the proxy forwards
// (satisfied by *config.MCPGuardConfig at runtime; declared here to
// keep internal/mcp free of the config import).
type ArgGuardKnobs interface {
	EffectiveEnabled() bool
	EffectiveMaxArgBytes() int
	EffectiveURLPolicy() string
}

// ArgGuardConfig carries the guard knobs (mirrors
// config.MCPGuardConfig without the config import).
type ArgGuardConfig struct {
	// Enabled master-switches the guard.
	Enabled bool
	// MaxArgBytes caps the marshaled argument size (0 means "apply
	// DefaultMaxArgBytes" when materialized via GuardConfigFromServer).
	MaxArgBytes int
	// URLPolicy is one of the URLPolicy* constants.
	URLPolicy string
	// AllowedHosts grants URL-policy exceptions: exact hostnames or
	// "*.suffix" wildcard entries (from the per-server config).
	AllowedHosts []string
}

// ValidateArgs checks tool-call arguments against the guard policy and
// returns every violation plus whether the call must be rejected.
// Structural violations (size cap, schema) always reject; URL flags
// reject under deny_private and only record under log. Args is the
// decoded JSON object from the model; schema is the tool's declared
// InputSchema rendered as generic JSON (may be nil). Unknown schema
// keywords are ignored per JSON Schema spec behavior.
func ValidateArgs(args map[string]any, schema any, cfg ArgGuardConfig) ([]ArgViolation, bool) {
	if !cfg.Enabled {
		return nil, false
	}
	switch cfg.URLPolicy {
	case URLPolicyLog, URLPolicyOff:
	default:
		// Fail closed: an unset or unknown policy in a directly-built
		// config must not silently degrade to collect-only.
		cfg.URLPolicy = URLPolicyDenyPrivate
	}
	if args == nil {
		// Absent arguments still face the schema (required properties,
		// URL walk): fail closed on a nil map.
		args = map[string]any{}
	}

	add := func(violations *[]ArgViolation, path, reason string) {
		*violations = append(*violations, ArgViolation{Path: path, Reason: reason})
	}

	var structural []ArgViolation
	if cfg.MaxArgBytes > 0 {
		encoded, err := json.Marshal(args)
		if err != nil {
			add(&structural, "$", "arguments are not valid JSON")
		} else if len(encoded) > cfg.MaxArgBytes {
			add(&structural, "$", fmt.Sprintf("arguments exceed the %d byte limit", cfg.MaxArgBytes))
		}
	}
	if len(structural) == 0 {
		structural = validateSchema(args, schema, "$", 0)
	}

	var urlFlags []ArgViolation
	if len(structural) == 0 {
		urlFlags = checkURLs(args, cfg, "$")
	}

	rejected := len(structural) > 0 || (cfg.URLPolicy == URLPolicyDenyPrivate && len(urlFlags) > 0)
	if !rejected {
		return append(structural, urlFlags...), false
	}
	return append(structural, urlFlags...), true
}

// validateSchema validates one value against one JSON-schema node.
// Supported keywords: type, enum, required, properties, items.
// Everything else is ignored (JSON Schema spec behavior).
func validateSchema(value any, schema any, path string, depth int) []ArgViolation {
	if depth > maxSchemaDepth {
		return []ArgViolation{{Path: path, Reason: fmt.Sprintf("schema nesting exceeds the %d level cap", maxSchemaDepth)}}
	}
	schemaMap, ok := schema.(map[string]any)
	if !ok {
		return nil
	}

	if value == nil {
		// Lenient JSON: models commonly send null for absent optional
		// properties. Null carries no content, so it validates as
		// absent unless the schema explicitly requires a null type.
		if !schemaAllowsNull(schemaMap) {
			return nil
		}
	}

	if t, ok := schemaMap["type"]; ok {
		if !matchesType(value, t) {
			return []ArgViolation{{Path: path, Reason: fmt.Sprintf("expected type %s", typeLabel(t))}}
		}
	}
	if enum, ok := schemaMap["enum"].([]any); ok && len(enum) > 0 {
		matched := false
		for _, member := range enum {
			if deepEqual(value, member) {
				matched = true
				break
			}
		}
		if !matched {
			return []ArgViolation{{Path: path, Reason: "value is not one of the allowed enum values"}}
		}
	}

	return append(validateSchemaObject(value, schemaMap, path, depth), validateSchemaArray(value, schemaMap, path, depth)...)
}

// validateSchemaObject applies the required/properties keywords to an
// object value.
func validateSchemaObject(value any, schemaMap map[string]any, path string, depth int) []ArgViolation {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	var violations []ArgViolation
	if required, ok := schemaMap["required"].([]any); ok {
		for _, r := range required {
			name, ok := r.(string)
			if !ok {
				continue
			}
			if _, present := obj[name]; !present {
				violations = append(violations, ArgViolation{Path: path, Reason: "missing required property " + name})
			}
		}
	}
	props, _ := schemaMap["properties"].(map[string]any)
	for name, propSchema := range props {
		if child, present := obj[name]; present {
			violations = append(violations, validateSchema(child, propSchema, joinPath(path, name), depth+1)...)
		}
	}
	return violations
}

// validateSchemaArray applies the items keyword to an array value.
func validateSchemaArray(value any, schemaMap map[string]any, path string, depth int) []ArgViolation {
	arr, ok := value.([]any)
	if !ok {
		return nil
	}
	items, ok := schemaMap["items"]
	if !ok {
		return nil
	}
	var violations []ArgViolation
	for i, element := range arr {
		violations = append(violations, validateSchema(element, items, fmt.Sprintf("%s[%d]", path, i), depth+1)...)
	}
	return violations
}

// schemaAllowsNull reports whether the schema's type keyword includes
// the null type (then a null value must still be checked against it).
func schemaAllowsNull(schemaMap map[string]any) bool {
	switch t := schemaMap["type"].(type) {
	case string:
		return t == "null"
	case []any:
		for _, member := range t {
			if name, ok := member.(string); ok && name == "null" {
				return true
			}
		}
	}
	return false
}

// matchesType reports whether value conforms to the schema "type"
// keyword (a string or an array of strings; integer accepts whole
// float64 values per JSON decoding).
func matchesType(value any, typeKeyword any) bool {
	switch t := typeKeyword.(type) {
	case string:
		return valueMatchesType(value, t)
	case []any:
		for _, member := range t {
			if name, ok := member.(string); ok && valueMatchesType(value, name) {
				return true
			}
		}
		return false
	default:
		// Unknown type keyword shape: ignore rather than mis-reject.
		return true
	}
}

func valueMatchesType(value any, typeName string) bool {
	switch typeName {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		n, ok := value.(float64)
		return ok && n == float64(int64(n))
	default:
		// Unknown type name: ignore rather than mis-reject.
		return true
	}
}

func typeLabel(typeKeyword any) string {
	switch t := typeKeyword.(type) {
	case string:
		return t
	case []any:
		names := make([]string, 0, len(t))
		for _, member := range t {
			if name, ok := member.(string); ok {
				names = append(names, name)
			}
		}
		return "one of [" + strings.Join(names, ", ") + "]"
	default:
		return "unknown"
	}
}

// deepEqual compares decoded JSON values; reflect.DeepEqual handles
// composite enum members (objects/arrays) safely — a plain == would
// panic on uncomparable types.
func deepEqual(a, b any) bool {
	return reflect.DeepEqual(a, b)
}

func joinPath(prefix, name string) string {
	if prefix == "$" {
		return name
	}
	return prefix + "." + name
}

// GuardConfigFromServer materializes an ArgGuardConfig from the
// governance knob (nil-safe) plus the per-server URL allowlist.
func GuardConfigFromServer(cfg ArgGuardKnobs, allowedHosts []string) ArgGuardConfig {
	if cfg == nil || !cfg.EffectiveEnabled() {
		return ArgGuardConfig{Enabled: false}
	}
	maxBytes := cfg.EffectiveMaxArgBytes()
	if maxBytes == 0 {
		maxBytes = DefaultMaxArgBytes
	}
	return ArgGuardConfig{
		Enabled:      true,
		MaxArgBytes:  maxBytes,
		URLPolicy:    cfg.EffectiveURLPolicy(),
		AllowedHosts: allowedHosts,
	}
}

// sanitizeViolationPath strips control characters from a model-supplied
// argument path (log-injection prevention) and caps its length.
func sanitizeViolationPath(path string) string {
	const maxPath = 128
	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 {
			return -1
		}
		return r
	}, path)
	if len(clean) > maxPath {
		for end := maxPath; end > 0; end-- {
			if utf8.RuneStart(clean[end]) {
				return clean[:end]
			}
		}
		return ""
	}
	return clean
}

// SummarizeArgViolations renders violations into a compact
// model-facing summary, capped so a pathological argument cannot flood
// the context.
func SummarizeArgViolations(violations []ArgViolation) string {
	const maxViolations = 5
	const maxSummary = 512
	capacity := len(violations)
	if capacity > maxViolations+1 {
		capacity = maxViolations + 1
	}
	parts := make([]string, 0, capacity)
	for i, v := range violations {
		if i == maxViolations {
			parts = append(parts, fmt.Sprintf("...and %d more", len(violations)-maxViolations))
			break
		}
		parts = append(parts, sanitizeViolationPath(v.Path)+": "+v.Reason)
	}
	summary := strings.Join(parts, "; ")
	if len(summary) > maxSummary {
		// Truncate on a rune boundary: paths embed schema/argument keys
		// that may be multi-byte.
		for end := maxSummary; end > 0; end-- {
			if utf8.RuneStart(summary[end]) {
				return summary[:end]
			}
		}
		return ""
	}
	return summary
}
