package mcp

import (
	"encoding/json"
	"testing"
)

// FuzzInputSchemaDecode pins the non-lossy tools/list decode (NENYA-172): a JSON
// object always decodes, and every top-level keyword of the decoded object
// survives into AsMap, so the sanitizer and the argument guard see the whole
// declaration.
func FuzzInputSchemaDecode(f *testing.F) {
	for _, seed := range []string{
		`{"type":"object","properties":{"a":{"type":"string"}}}`,
		`{"$ref":"#/$defs/A","$defs":{"A":{"type":"string"}}}`,
		`{"additionalProperties":{"type":"string"}}`,
		`{"oneOf":[{"required":["a"]}],"items":[{"type":"string"}]}`,
		`{}`,
		`null`,
		`[]`,
		`"x"`,
		`{"patternProperties":{"^x":{"type":"string"}}}`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, input string) {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(input), &decoded); err != nil {
			return
		}
		var schema InputSchema
		if err := json.Unmarshal([]byte(input), &schema); err != nil {
			// A JSON object must always decode: an error here would drop the
			// whole tools/list result (the lossy-decode class).
			t.Fatalf("decoding a JSON object failed: %v", err)
		}
		asMap := schema.AsMap()
		if len(decoded) == 0 {
			return
		}
		for key := range decoded {
			if _, ok := asMap[key]; !ok {
				t.Fatalf("keyword %q was lost by the decode: %s", key, mustJSONMap(asMap))
			}
		}
	})
}

func mustJSONMap(value map[string]any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "<unmarshalable>"
	}
	return string(encoded)
}
