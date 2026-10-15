package jsonschema

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// schemaVariantCorpus is the NENYA-172 adversarial corpus: every upstream
// schema-sanitization fix class as a fixture with its expected sanitized shape.
type schemaVariantCorpus struct {
	Cases []struct {
		Name       string         `json:"name"`
		Schema     any            `json:"schema"`
		Absent     []string       `json:"absent"`
		AbsentText []string       `json:"absent_text"`
		Present    map[string]any `json:"present"`
		Type       string         `json:"type"`
	} `json:"cases"`
}

// TestSchemaVariantTaxonomy runs the committed corpus against the sanitizer.
func TestSchemaVariantTaxonomy(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "schema_variants.json"))
	if err != nil {
		t.Fatalf("reading corpus: %v", err)
	}
	var corpus schemaVariantCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parsing corpus: %v", err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("corpus is empty")
	}

	for _, tc := range corpus.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			out := Sanitize(tc.Schema)
			if out == nil {
				t.Fatal("Sanitize returned nil")
			}
			for _, keyword := range tc.Absent {
				if found := findKeyword(out, keyword, 0); found {
					t.Fatalf("keyword %q survived: %s", keyword, canonicalJSON(out))
				}
			}
			encoded := canonicalJSON(out)
			for _, text := range tc.AbsentText {
				// The check walks the decoded strings: json.Marshal escapes
				// control characters, so a substring search on the encoded form
				// would be vacuous.
				for _, field := range stringsIn(out, 0) {
					if strings.Contains(field, text) {
						t.Fatalf("text %q survived in %q: %s", text, field, encoded)
					}
				}
			}
			for key, want := range tc.Present {
				got, ok := out[key]
				if !ok {
					t.Fatalf("key %q missing from %s", key, encoded)
				}
				if !jsonEqual(got, want) {
					t.Fatalf("key %q = %s, want %s", key, mustJSON(got), mustJSON(want))
				}
			}
			if tc.Type != "" {
				if got, _ := out["type"].(string); got != tc.Type {
					t.Fatalf("type = %q, want %q (%s)", got, tc.Type, encoded)
				}
			}
		})
	}
}

// stringsIn collects every string in a decoded tree (used for the control
// character assertions, which json.Marshal escaping would otherwise hide).
func stringsIn(node any, depth int) []string {
	if depth > 64 {
		return nil
	}
	var out []string
	switch typed := node.(type) {
	case string:
		out = append(out, typed)
	case map[string]any:
		for key, value := range typed {
			out = append(out, key)
			out = append(out, stringsIn(value, depth+1)...)
		}
	case []any:
		for _, item := range typed {
			out = append(out, stringsIn(item, depth+1)...)
		}
	}
	return out
}

// findKeyword reports whether a keyword appears anywhere in the sanitized tree.
func findKeyword(node any, keyword string, depth int) bool {
	if depth > 64 {
		return false
	}
	switch typed := node.(type) {
	case map[string]any:
		for key, value := range typed {
			if key == keyword {
				return true
			}
			if findKeyword(value, keyword, depth+1) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if findKeyword(item, keyword, depth+1) {
				return true
			}
		}
	}
	return false
}

func jsonEqual(a, b any) bool {
	return reflect.DeepEqual(normalizeJSON(a), normalizeJSON(b))
}

func normalizeJSON(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var out any
	if err := json.Unmarshal(encoded, &out); err != nil {
		return value
	}
	return out
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "<unmarshalable>"
	}
	return string(encoded)
}

// TestSanitizeDepthCap pins that a pathologically nested schema is truncated to
// the empty schema instead of failing or exhausting the stack.
func TestSanitizeDepthCap(t *testing.T) {
	schema := map[string]any{"type": "object"}
	node := schema
	for i := 0; i < maxDepth*3; i++ {
		child := map[string]any{"type": "object"}
		node["properties"] = map[string]any{"deep": child}
		node = child
	}
	out := Sanitize(schema)
	if out == nil {
		t.Fatal("Sanitize returned nil")
	}
	// schemaDepth counts the root as level 0, so the deepest emitted node (which
	// is sanitized at exactly maxDepth) reads as maxDepth+1.
	if deepest := schemaDepth(out, 0); deepest > maxDepth+1 {
		t.Fatalf("sanitized depth = %d, want <= %d", deepest, maxDepth+1)
	}
}

// countNodes counts the maps and slices in a sanitized schema.
func countNodes(node any, depth int) int {
	if depth > 128 {
		return 0
	}
	switch typed := node.(type) {
	case map[string]any:
		count := 1
		for _, value := range typed {
			count += countNodes(value, depth+1)
		}
		return count
	case []any:
		count := 1
		for _, item := range typed {
			count += countNodes(item, depth+1)
		}
		return count
	}
	return 0
}

// schemaDepth measures the deepest properties nesting of a sanitized schema.
func schemaDepth(node any, depth int) int {
	table, ok := node.(map[string]any)
	if !ok {
		return depth
	}
	props, ok := table["properties"].(map[string]any)
	if !ok {
		return depth
	}
	deepest := depth
	for _, child := range props {
		if d := schemaDepth(child, depth+1); d > deepest {
			deepest = d
		}
	}
	return deepest
}

// TestSanitizePropertiesCap pins the maxProperties bound on a single object
// schema.
func TestSanitizePropertiesCap(t *testing.T) {
	props := make(map[string]any, maxProperties+10)
	for i := 0; i < maxProperties+10; i++ {
		props[fmt.Sprintf("p%d", i)] = map[string]any{"type": "string"}
	}
	out := Sanitize(map[string]any{"type": "object", "properties": props})
	got, _ := out["properties"].(map[string]any)
	if len(got) > maxProperties {
		t.Fatalf("properties = %d, want <= %d", len(got), maxProperties)
	}
}

// TestSanitizeToolName pins the charset, the length cap, and determinism of the
// tool-name normalization.
func TestSanitizeToolName(t *testing.T) {
	long := strings.Repeat("very-long-tool-name-", 10)
	capped := SanitizeToolName(long)
	if len(capped) > maxToolNameLen {
		t.Fatalf("name length = %d, want <= %d", len(capped), maxToolNameLen)
	}
	if again := SanitizeToolName(long); again != capped {
		t.Fatalf("name mapping is not deterministic: %q vs %q", capped, again)
	}
	// Two long names that share a prefix must not collide after truncation.
	other := long + "-different"
	if SanitizeToolName(other) == capped {
		t.Fatal("two distinct long names collapsed to the same capped name")
	}
	if got := SanitizeToolName("weird name/with:chars"); got != "weird_name_with_chars" {
		t.Fatalf("charset sanitization = %q", got)
	}
	if got := SanitizeToolName(""); got != "tool" {
		t.Fatalf("empty name = %q, want tool", got)
	}
	if got := SanitizeToolName("ok-name_1"); got != "ok-name_1" {
		t.Fatalf("valid name changed: %q", got)
	}
}

// TestSanitizeRefExpansionIsBounded pins the NENYA-172 expansion bound: a
// definition graph whose entries reference each other (memoized, so each is
// expanded once) and a long alias chain both stay bounded instead of exploding
// or hanging.
func TestSanitizeRefExpansionIsBounded(t *testing.T) {
	// A definition with N properties, each referencing it: without memoization
	// this multiplies out; with it, one expansion.
	defs := map[string]any{"A": map[string]any{"type": "object", "properties": map[string]any{}}}
	props := map[string]any{}
	for i := 0; i < 40; i++ {
		props[fmt.Sprintf("p%d", i)] = map[string]any{"$ref": "#/$defs/A"}
	}
	defs["A"].(map[string]any)["properties"] = props
	schema := map[string]any{"type": "object", "properties": map[string]any{"root": map[string]any{"$ref": "#/$defs/A"}}, "$defs": defs}
	done := make(chan map[string]any, 1)
	go func() { done <- Sanitize(schema) }()
	select {
	case out := <-done:
		if out == nil {
			t.Fatal("Sanitize returned nil")
		}
		if findKeyword(out, "$ref", 0) {
			t.Fatalf("a $ref survived: %s", canonicalJSON(out))
		}
		// Memoization is what bounds the expansion: without it the shared
		// definition multiplies out (40 properties each re-expanding the same
		// definition). The emitted node count must stay proportional to the
		// definition, not to the product.
		if nodes := countNodes(out, 0); nodes > 4*len(props)+16 {
			t.Fatalf("expansion emitted %d nodes for %d referencing properties", nodes, len(props))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Sanitize did not terminate on a self-referential definition graph")
	}

	// A long alias chain: every hop is inlined until the hop cap, after which the
	// node becomes the empty schema (never an infinite expansion).
	chain := map[string]any{}
	for i := 0; i < 30; i++ {
		chain[fmt.Sprintf("D%d", i)] = map[string]any{"$ref": fmt.Sprintf("#/$defs/D%d", i+1)}
	}
	chain["D30"] = map[string]any{"type": "string"}
	out := Sanitize(map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"$ref": "#/$defs/D0"}}, "$defs": chain})
	if findKeyword(out, "$ref", 0) {
		t.Fatalf("a $ref survived the chain: %s", canonicalJSON(out))
	}
}

// TestSanitizeNestedDefinitionScope pins that a $ref resolves against the
// nearest enclosing definition table, not only the root's.
func TestSanitizeNestedDefinitionScope(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{
				"$defs": map[string]any{"X": map[string]any{"type": "STRING", "minLength": 1}},
				"$ref":  "#/$defs/X",
			},
		},
		"$defs": map[string]any{"Y": map[string]any{"type": "integer"}},
	}
	out := Sanitize(schema)
	props, _ := out["properties"].(map[string]any)
	a, _ := props["a"].(map[string]any)
	if a["type"] != "string" {
		t.Fatalf("a nested $defs reference did not resolve: %s", canonicalJSON(out))
	}
}

// TestSanitizeIsIdempotent pins that sanitizing a sanitized schema is a no-op,
// which is what lets the sanitizer run at every hop without churn.
func TestSanitizeIsIdempotent(t *testing.T) {
	schema := map[string]any{
		"$schema": "x",
		"type":    "OBJECT",
		"properties": map[string]any{
			"a": map[string]any{"$ref": "#/$defs/A"},
			"b": map[string]any{"type": "string", "description": "x\u0000y", "pattern": "^\\p{L}+$"},
		},
		"$defs": map[string]any{"A": map[string]any{"type": "STRING"}},
	}
	once := Sanitize(schema)
	twice := Sanitize(once)
	if canonicalJSON(once) != canonicalJSON(twice) {
		t.Fatalf("not idempotent:\n%s\n%s", canonicalJSON(once), canonicalJSON(twice))
	}
}
