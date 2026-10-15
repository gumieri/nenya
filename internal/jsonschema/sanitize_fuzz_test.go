package jsonschema

import (
	"encoding/json"
	"strings"
	"testing"
)

// FuzzSanitizeToolSchema exercises the schema sanitizer over arbitrary JSON
// (NENYA-172): malformed regexes, deep nesting, unicode escapes, boolean
// schemas, $ref cycles, and NUL escapes. The sanitizer must be total (never
// panic, never return nil), must strip every dialect/definition keyword and
// every NUL/C0 control, must leave no $ref behind, must stay within the depth
// bound, and must be idempotent — a sanitized schema is a fixed point, which is
// what lets the sanitizer run at every hop.
func FuzzSanitizeToolSchema(f *testing.F) {
	for _, seed := range []string{
		`{"type":"object","properties":{"a":{"type":"string"}}}`,
		`{"$schema":"x","$id":"y","type":"OBJECT"}`,
		`{"type":"object","properties":{"a":{"$ref":"#/$defs/A"}},"$defs":{"A":{"type":"STRING"}}}`,
		`{"type":"object","properties":{"a":{"$ref":"#/$defs/L"}},"$defs":{"L":{"properties":{"self":{"$ref":"#/$defs/L"}}}}}`,
		`{"type":"object","properties":{"a":{"pattern":"^\p{L}+$"}}}`,
		`{"type":"object","properties":{"a":{"pattern":"^([a-z$"}}}`,
		`{"type":"object","properties":{"a":true,"b":false}}`,
		`{"type":"object","additionalProperties":false}`,
		`{"type":"object","properties":{"a":{"description":"x\u0000y"}}}`,
		`true`,
		`false`,
		`null`,
		`"not a schema"`,
		`[]`,
		`{"patternProperties":{"^\p{L}+$":{"type":"string"}}}`,
		`{"type":["STRING","NULL"]}`,
		`{"type":"object","properties":{"a":{"items":[{"type":"STRING"}]}}}`,
		// Found by the fuzzer: a $ref nested inside an unknown keyword's value,
		// a DEL character in a keyword name, and control characters in a pattern
		// and a required name.
		`{"":{"":{"":{"":{"$ref":""}}}}}`,
		"{\"\u007f\":\"\"}",
		`{"properties":{"a":{"pattern":"^\b00000"}}}`,
		`{"type":"object","required":["a\u0000b"]}`,
		// Found by the fuzzer: an uninterpretable type (numeric, mixed array),
		// a control character in a type name, and a required list with no
		// declared properties (pruning must not break idempotence).
		`{"properties":{"a":{"type":5}}}`,
		`{"properties":{"a":{"type":["STRING",5]}}}`,
		`{"properties":{"a":{"type":"\u007f"}}}`,
		`{"properties":{"a":{"type":""}}}`,
		`{"properties":{"a":{"type":" "}}}`,
		`{"properties":{"a":{"type":"foo"}}}`,
		`{"properties":{"a":{"type":" OBJECT "}}}`,
		`{"type":"object","required":["ab"]}`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, input string) {
		var decoded any
		if err := json.Unmarshal([]byte(input), &decoded); err != nil {
			return
		}
		out := Sanitize(decoded)
		if out == nil {
			t.Fatal("Sanitize returned nil")
		}
		encoded := canonicalJSON(out)
		for keyword := range dialectKeywords {
			if findKeyword(out, keyword, 0) {
				t.Fatalf("dialect keyword %q survived: %s", keyword, encoded)
			}
		}
		for keyword := range definitionKeywords {
			if findKeyword(out, keyword, 0) {
				t.Fatalf("definition keyword %q survived: %s", keyword, encoded)
			}
		}
		if findKeyword(out, "$ref", 0) {
			t.Fatalf("$ref survived: %s", encoded)
		}
		// The check walks the decoded strings: json.Marshal escapes control
		// characters, so a substring search on the encoded form would be vacuous.
		for _, text := range stringsIn(out, 0) {
			for _, r := range text {
				if isStrippedControl(r) {
					t.Fatalf("control character %q survived in %q: %s", r, text, encoded)
				}
			}
		}
		if depth := schemaDepth(out, 0); depth > maxDepth+2 {
			t.Fatalf("depth = %d, want <= %d", depth, maxDepth+2)
		}
		if again := canonicalJSON(Sanitize(out)); again != encoded {
			t.Fatalf("not idempotent:\n%s\n%s", encoded, again)
		}
	})
}

// FuzzSanitizeToolName exercises the tool-name normalization: the result is
// always non-empty, inside the charset, within the length cap, and deterministic.
func FuzzSanitizeToolName(f *testing.F) {
	for _, seed := range []string{"server__tool", "weird name/with:chars", "", "a", strings.Repeat("x", 200)} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, name string) {
		got := SanitizeToolName(name)
		if got == "" {
			t.Fatal("empty sanitized name")
		}
		if len(got) > maxToolNameLen {
			t.Fatalf("name length = %d, want <= %d", len(got), maxToolNameLen)
		}
		for _, r := range got {
			valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
			if !valid {
				t.Fatalf("invalid character %q in %q", r, got)
			}
		}
		if again := SanitizeToolName(name); again != got {
			t.Fatalf("non-deterministic: %q vs %q", got, again)
		}
	})
}
