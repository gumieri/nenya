package jsonschema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// maxDepth caps subschema recursion. A deeper subtree is replaced by the
	// empty schema ({}), which every provider accepts, instead of failing.
	maxDepth = 32
	// maxRefHops caps $ref expansion hops, so a self-referential or mutually
	// recursive $defs graph cannot expand forever.
	maxRefHops = 8
	// maxNodes caps the total subschema nodes a single sanitize call may emit.
	// $ref expansion is memoized, so a hostile definition graph cannot multiply
	// its way past this budget: past it, subtrees degrade to {}.
	maxNodes = 20000
	// maxProperties caps the properties a single object schema may declare.
	maxProperties = 1024
	// maxToolNameLen is the strictest provider function-name limit (OpenAI's 64).
	maxToolNameLen = 64
)

// dialectKeywords are meta/dialect keys that providers reject and that carry no
// validation meaning for a tool argument schema. They are stripped everywhere.
var dialectKeywords = map[string]bool{
	"$schema":          true,
	"$id":              true,
	"$anchor":          true,
	"$dynamicAnchor":   true,
	"$dynamicRef":      true,
	"$recursiveAnchor": true,
	"$recursiveRef":    true,
	"$vocabulary":      true,
	"$comment":         true,
}

// definitionKeywords hold a definition table. The table is read (to resolve
// local $ref pointers) and is NOT emitted: providers reject unknown root
// keywords, and every resolved pointer has been inlined.
var definitionKeywords = map[string]bool{"$defs": true, "definitions": true}

var (
	// subschemaKeyword keywords hold one subschema.
	subschemaKeyword = map[string]bool{"not": true, "if": true, "then": true, "else": true, "contains": true, "propertyNames": true, "contentSchema": true, "additionalItems": true}
	// boolOrSubschemaKeyword keywords accept a meaningful boolean as well as a
	// subschema: `additionalProperties: false` is a real constraint and must be
	// preserved (only a bare boolean in a pure-subschema position is widened).
	boolOrSubschemaKeyword = map[string]bool{"additionalProperties": true, "unevaluatedProperties": true, "unevaluatedItems": true}
	// subschemaListKw keywords hold a list of subschemas.
	subschemaListKw = map[string]bool{"allOf": true, "anyOf": true, "oneOf": true, "prefixItems": true}
	// subschemaMapKw keywords hold a map of subschemas.
	subschemaMapKw = map[string]bool{"properties": true, "patternProperties": true, "dependentSchemas": true}
	// unicodePropertyEscapeRe matches a Unicode property escape (\p{...},
	// \P{...}), the documented provider-400 class and the upstream bypass.
	unicodePropertyEscapeRe = regexp.MustCompile(`\\[pP]\{`)
	// ecmaOnlyConstructRe matches a regex construct that RE2 does not implement
	// but ECMA-262 (and therefore most provider validators) accepts, so a
	// pattern using one is forwarded instead of dropped.
	ecmaOnlyConstructRe = regexp.MustCompile(`\(\?<?[=!]|\\[1-9]|\(\?<[A-Za-z]`)
)

// Sanitize normalizes a decoded tool schema into a provider-safe object schema.
// A nil, non-object, or boolean input becomes the minimal object schema, so a
// parameterless tool still gets a valid declaration. The root is always an
// object schema: tool arguments are a JSON object, and providers require
// `input_schema.type == "object"`.
func Sanitize(schema any) map[string]any {
	s := &sanitizer{memo: make(map[string]map[string]any)}
	root := s.node(nil, schema, 0, 0)
	if len(root) == 0 {
		return emptyObjectSchema()
	}
	root["type"] = "object"
	if _, ok := root["properties"].(map[string]any); !ok {
		if _, hasPatterns := root["patternProperties"].(map[string]any); !hasPatterns {
			root["properties"] = map[string]any{}
		}
	}
	return root
}

// pruneRequired drops required names the schema does not declare: a property
// dropped by the caps (or a malformed required list) would otherwise name a
// property no provider can see, which some providers reject.
func pruneRequired(schema map[string]any) {
	required, ok := schema["required"].([]any)
	if !ok {
		return
	}
	props, hasProps := schema["properties"].(map[string]any)
	if !hasProps || len(props) == 0 {
		// With no declared properties there is nothing to prune against: a
		// required list is legal on its own, and pruning it would make the
		// sanitizer non-idempotent.
		return
	}
	kept := make([]any, 0, len(required))
	for _, name := range required {
		if text, ok := name.(string); ok {
			if _, declared := props[text]; !declared {
				continue
			}
		}
		kept = append(kept, name)
	}
	if len(kept) == 0 {
		delete(schema, "required")
		return
	}
	schema["required"] = kept
}

// emptyObjectSchema is the minimal valid tool schema.
func emptyObjectSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// scope is the definition table in effect at a node (the nearest enclosing
// $defs/definitions merged over the outer scope).
type scope struct {
	table map[string]any
	gen   int
}

// sanitizer carries the per-call state: the node budget and the memoized
// sanitized definition entries (so a definition referenced N times is expanded
// once, which bounds the expansion cost of a hostile definition graph).
type sanitizer struct {
	nodes int
	gen   int
	memo  map[string]map[string]any
}

// node normalizes one subschema.
func (s *sanitizer) node(defs *scope, value any, depth, refs int) map[string]any {
	s.nodes++
	if depth > maxDepth || s.nodes > maxNodes {
		return map[string]any{}
	}
	source, ok := value.(map[string]any)
	if !ok {
		// A boolean subschema is legal JSON Schema but rejected by providers:
		// true accepts anything, false accepts nothing. Both widen to the empty
		// schema (the argument guard, not this sanitizer, is the enforcement
		// point for the declared subset).
		return map[string]any{}
	}

	local := extendScope(defs, source, &s.gen)

	out := make(map[string]any, len(source))
	// A local $ref is resolved by inlining the target; an unresolvable or cyclic
	// pointer contributes nothing (the node's own keywords below win over the
	// resolved ones, as JSON Schema 2020-12 allows siblings).
	if resolved, ok := s.resolveRef(local, source, refs); ok {
		for key, resolvedValue := range resolved {
			out[key] = resolvedValue
		}
	}
	for rawKey, rawValue := range source {
		key := stripControlChars(rawKey)
		if key == "" || dialectKeywords[key] || definitionKeywords[key] || key == "$ref" {
			continue
		}
		if sanitized, keep := s.keyword(local, key, rawValue, depth, refs); keep {
			out[key] = sanitized
		}
	}

	out = normalizeType(out)
	pruneRequired(out)
	return out
}

// keyword normalizes one keyword value, reporting keep=false for a keyword that
// must be dropped (a non-schema value in a schema position, or a regex the
// provider's flavor cannot compile).
func (s *sanitizer) keyword(defs *scope, key string, value any, depth, refs int) (any, bool) {
	switch {
	case boolOrSubschemaKeyword[key]:
		if _, isBool := value.(bool); isBool {
			return value, true
		}
		return s.subschema(defs, value, depth, refs)
	case subschemaKeyword[key]:
		return s.subschema(defs, value, depth, refs)
	case subschemaListKw[key]:
		return s.subschemaList(defs, value, depth, refs)
	case subschemaMapKw[key]:
		table, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		return s.subschemaTable(defs, key, table, depth, refs), true
	case key == "items":
		return s.items(defs, value, depth, refs)
	case key == "pattern":
		pattern, ok := value.(string)
		if !ok {
			return nil, false
		}
		pattern = stripControlChars(pattern)
		return pattern, patternAllowed(pattern)
	case key == "required":
		return sanitizeRequired(value)
	case key == "type":
		normalized := normalizedTypeValue(value)
		if normalized == nil {
			// An uninterpretable type (a number, an object, an array with
			// non-string members) is dropped rather than forwarded as null,
			// which would be invalid JSON Schema and a 400 source.
			return nil, false
		}
		return normalized, true
	default:
		return sanitizeValue(value, depth+1), true
	}
}

// subschema normalizes a single-subschema keyword value.
func (s *sanitizer) subschema(defs *scope, value any, depth, refs int) (any, bool) {
	if _, isBool := value.(bool); isBool {
		return map[string]any{}, true
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, false
	}
	return s.node(defs, value, depth+1, refs), true
}

// subschemaList normalizes a list-of-subschemas keyword value.
func (s *sanitizer) subschemaList(defs *scope, value any, depth, refs int) (any, bool) {
	list, ok := value.([]any)
	if !ok {
		return nil, false
	}
	sanitized := make([]any, 0, len(list))
	for _, item := range list {
		if _, isBool := item.(bool); isBool {
			sanitized = append(sanitized, map[string]any{})
			continue
		}
		if _, ok := item.(map[string]any); !ok {
			continue
		}
		sanitized = append(sanitized, s.node(defs, item, depth+1, refs))
	}
	return sanitized, true
}

// subschemaTable walks a map-of-subschemas keyword. A patternProperties key is a
// regex: an entry whose pattern the provider's flavor cannot compile is dropped
// instead of failing the whole request. Property names have control characters
// stripped (a documented 400 source), and the table is emitted in sorted order
// with a hard cap so the result is deterministic.
func (s *sanitizer) subschemaTable(defs *scope, keyword string, table map[string]any, depth, refs int) map[string]any {
	names := sortedKeys(table)
	out := make(map[string]any, len(names))
	for _, rawName := range names {
		if len(out) >= maxProperties {
			break
		}
		name := stripControlChars(rawName)
		if name == "" {
			continue
		}
		if keyword == "patternProperties" && !patternAllowed(name) {
			continue
		}
		value := table[rawName]
		if _, isBool := value.(bool); isBool {
			out[name] = map[string]any{}
			continue
		}
		if _, ok := value.(map[string]any); !ok {
			continue
		}
		out[name] = s.node(defs, value, depth+1, refs)
	}
	return out
}

// items normalizes the items keyword in every accepted form (a single schema, a
// boolean, or the pre-2020 tuple array).
func (s *sanitizer) items(defs *scope, value any, depth, refs int) (any, bool) {
	if list, ok := value.([]any); ok {
		sanitized := make([]any, 0, len(list))
		for _, item := range list {
			if _, isBool := item.(bool); isBool {
				sanitized = append(sanitized, map[string]any{})
				continue
			}
			if _, ok := item.(map[string]any); !ok {
				continue
			}
			sanitized = append(sanitized, s.node(defs, item, depth+1, refs))
		}
		return sanitized, true
	}
	return s.subschema(defs, value, depth, refs)
}

// extendScope merges a node's own definition table over the enclosing scope
// (deterministically: $defs wins over definitions, and an outer name survives
// unless the inner table redefines it). gen advances when a new table appears,
// so memoized entries never leak across scopes.
func extendScope(outer *scope, node map[string]any, gen *int) *scope {
	if !hasDefinitionTable(node) {
		if outer != nil {
			return outer
		}
		return &scope{}
	}
	*gen++
	merged := make(map[string]any)
	if outer != nil {
		for name, value := range outer.table {
			merged[name] = value
		}
	}
	// Fixed precedence order, so the merge is deterministic.
	for _, keyword := range []string{"definitions", "$defs"} {
		table, ok := node[keyword].(map[string]any)
		if !ok {
			continue
		}
		for name, value := range table {
			merged[name] = value
		}
	}
	return &scope{table: merged, gen: *gen}
}

// hasDefinitionTable reports whether a node carries its own definition table.
func hasDefinitionTable(node map[string]any) bool {
	for keyword := range definitionKeywords {
		if _, ok := node[keyword]; ok {
			return true
		}
	}
	return false
}

// The returned map shares nested maps with the memoized expansion (and therefore
// between insertion points): treat a sanitized schema as READ-ONLY.
//
// resolveRef expands a local $ref pointer (#/$defs/NAME or #/definitions/NAME,
// including nested JSON-pointer paths) into a sanitized schema. ok is false for
// an external, malformed, or cyclic pointer. The expansion is memoized per
// scope generation and name, so a definition referenced many times costs one
// sanitize pass.
func (s *sanitizer) resolveRef(defs *scope, node map[string]any, refs int) (map[string]any, bool) {
	ref, ok := node["$ref"].(string)
	if !ok {
		return nil, false
	}
	if refs >= maxRefHops {
		return nil, false
	}
	name, target, ok := lookupRef(ref, defs)
	if !ok {
		return nil, false
	}
	memoKey := fmt.Sprintf("%d/%s", defs.gen, name)
	if cached, ok := s.memo[memoKey]; ok {
		return cached, true
	}
	// Publish a placeholder BEFORE recursing: a self-referential definition then
	// sees the empty schema instead of expanding forever, and the memo makes the
	// cost one expansion per definition (a definition referenced N times used to
	// multiply out).
	s.memo[memoKey] = map[string]any{}
	resolved := s.node(defs, target, 0, refs+1)
	s.memo[memoKey] = resolved
	return resolved, true
}

// lookupRef resolves a local JSON pointer against the scope's definition table,
// returning the definition name (for memoization) and its value.
func lookupRef(ref string, defs *scope) (string, any, bool) {
	if defs == nil || !strings.HasPrefix(ref, "#/") {
		return "", nil, false
	}
	segments := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
	if len(segments) == 0 {
		return "", nil, false
	}
	switch segments[0] {
	case "$defs", "definitions":
		segments = segments[1:]
	default:
		return "", nil, false
	}
	if len(segments) == 0 {
		return "", nil, false
	}
	var current any = defs.table
	for _, segment := range segments {
		table, ok := current.(map[string]any)
		if !ok {
			return "", nil, false
		}
		next, ok := table[unescapePointer(segment)]
		if !ok {
			return "", nil, false
		}
		current = next
	}
	return strings.Join(segments, "/"), current, true
}

// unescapePointer decodes the ~0/~1 JSON-pointer escapes.
func unescapePointer(segment string) string {
	segment = strings.ReplaceAll(segment, "~1", "/")
	return strings.ReplaceAll(segment, "~0", "~")
}

// patternAllowed reports whether a regex pattern is safe to forward. The
// documented drop class is a Unicode property escape (rejected by several
// providers and the upstream bypass). A pattern that RE2 cannot compile is
// dropped only when it uses no ECMA-only construct (a lookaround, a back
// reference, or a named group), so a valid ECMA-262 pattern is never lost;
// the residual is that a pattern RE2 accepts but a provider's flavor rejects is
// forwarded and the provider answers with its own 400.
func patternAllowed(pattern string) bool {
	if pattern == "" {
		return false
	}
	if unicodePropertyEscapeRe.MatchString(pattern) {
		return false
	}
	if ecmaOnlyConstructRe.MatchString(pattern) {
		return true
	}
	_, err := regexp.Compile(pattern)
	return err == nil
}

// normalizedTypeValue lowercases a declared type (the string or array form) and
// drops an uninterpretable value.
func normalizedTypeValue(value any) any {
	switch typed := value.(type) {
	case string:
		name := normalizeTypeName(typed)
		if name == "" {
			// An empty, whitespace-only, control-only, or unknown type name is
			// invalid JSON Schema and a documented 400 source: drop it (the
			// shape then infers a type or declares none) rather than forward
			// `"type": ""`.
			return nil
		}
		return name
	case []any:
		lowered := make([]any, 0, len(typed))
		for _, member := range typed {
			name, ok := member.(string)
			if !ok {
				// A mixed array is not a valid type declaration.
				return nil
			}
			name = normalizeTypeName(name)
			if name == "" {
				return nil
			}
			lowered = append(lowered, name)
		}
		if len(lowered) == 0 {
			return nil
		}
		return lowered
	default:
		return nil
	}
}

// jsonSchemaTypes is the closed set of JSON Schema type names. A declared type
// outside it (an unknown name, a whitespace-only string, a control-only string)
// is a provider-400 shape, so it is dropped.
var jsonSchemaTypes = map[string]bool{
	"object": true, "array": true, "string": true, "number": true,
	"integer": true, "boolean": true, "null": true,
}

// normalizeTypeName lowercases a type name, trims surrounding whitespace, and
// strips control characters, returning "" when the result is not a JSON Schema
// type name.
func normalizeTypeName(name string) string {
	name = strings.TrimSpace(strings.ToLower(stripControlChars(name)))
	if !jsonSchemaTypes[name] {
		return ""
	}
	return name
}

// normalizeType infers an object/array type from the schema's shape when it
// declares none. Uppercase type names are accepted by some providers and
// rejected by others, and a schema with properties but no type is rejected by
// most. A declared type has already been lowercased by keyword.
func normalizeType(schema map[string]any) map[string]any {
	if _, ok := schema["type"]; ok {
		return schema
	}
	if _, ok := schema["properties"]; ok {
		schema["type"] = "object"
		return schema
	}
	if _, ok := schema["patternProperties"]; ok {
		schema["type"] = "object"
		return schema
	}
	if _, ok := schema["items"]; ok {
		schema["type"] = "array"
		return schema
	}
	if _, ok := schema["prefixItems"]; ok {
		schema["type"] = "array"
	}
	return schema
}

// sanitizeRequired normalizes a required list: every name has control
// characters stripped, and an entry that becomes empty is dropped.
func sanitizeRequired(value any) (any, bool) {
	list, ok := value.([]any)
	if !ok {
		return nil, false
	}
	kept := make([]any, 0, len(list))
	for _, entry := range list {
		name, ok := entry.(string)
		if !ok {
			continue
		}
		name = stripControlChars(name)
		if name == "" {
			continue
		}
		kept = append(kept, name)
	}
	return kept, true
}

// sanitizeValue recursively strips NUL and C0 control characters (except tab,
// newline, and carriage return) from every string in a non-schema value, and
// removes a nested $ref/dialect/definition key: a provider that tries to resolve
// one fails the request, and an unknown keyword's value carries no meaning to it
// (the upstream $ref-removal class).
func sanitizeValue(value any, depth int) any {
	if depth > maxDepth {
		return nil
	}
	switch typed := value.(type) {
	case string:
		return stripControlChars(typed)
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, sanitizeValue(item, depth+1))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for rawKey, item := range typed {
			key := stripControlChars(rawKey)
			if key == "" || dialectKeywords[key] || definitionKeywords[key] || key == "$ref" {
				continue
			}
			out[key] = sanitizeValue(item, depth+1)
		}
		return out
	default:
		return value
	}
}

// stripControlChars removes NUL, DEL, and the C0 controls that are not
// whitespace (tab/newline/CR survive as ordinary JSON escapes).
func stripControlChars(text string) string {
	if !strings.ContainsFunc(text, isStrippedControl) {
		return text
	}
	var builder strings.Builder
	builder.Grow(len(text))
	for _, r := range text {
		if isStrippedControl(r) {
			continue
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

// isStrippedControl reports whether a rune is a NUL/DEL/C0 control that must be
// removed from a schema string.
func isStrippedControl(r rune) bool {
	if r == 0x7f {
		return true
	}
	if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
		return true
	}
	return false
}

// sortedKeys returns a map's keys in ascending order, so every emitted table is
// deterministic (a hostile schema cannot make the sanitized output vary between
// runs, which would churn the request prefix and defeat caching).
func sortedKeys(table map[string]any) []string {
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// SanitizeToolName normalizes a tool name for a provider's function-name
// charset and length cap: every character outside [A-Za-z0-9_-] becomes '_', the
// name is capped at maxToolNameLen with a deterministic hash suffix, and an empty
// result becomes "tool". Deterministic, so the same input always maps to the
// same name (the registry keys on it).
func SanitizeToolName(name string) string {
	return CapToolName(sanitizeNameChars(name))
}

// sanitizeNameChars replaces every character outside the function-name charset.
func sanitizeNameChars(name string) string {
	var builder strings.Builder
	builder.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	out := strings.Trim(builder.String(), "_")
	if out == "" {
		return "tool"
	}
	return out
}

// CapToolName truncates a name to maxToolNameLen (on a rune boundary), keeping a
// deterministic 8-hex-character digest of the ORIGINAL name as a suffix so two
// long names cannot collide after truncation.
func CapToolName(name string) string {
	if utf8.RuneCountInString(name) <= maxToolNameLen {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	keep := maxToolNameLen - len(suffix)
	prefix := name
	for utf8.RuneCountInString(prefix) > keep {
		_, size := utf8.DecodeLastRuneInString(prefix)
		prefix = prefix[:len(prefix)-size]
	}
	return prefix + suffix
}

// canonicalJSON renders a sanitized schema deterministically (json.Marshal sorts
// map keys); the tests use it to compare whole schemas.
func canonicalJSON(schema map[string]any) string {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}
