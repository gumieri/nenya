package util

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestExtractTopLevelField covers the semantics matrix: presence, value
// fidelity, duplicate keys (last wins), non-object top levels, and
// malformed JSON.
func TestExtractTopLevelField(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		field     string
		wantFound bool
		wantValue string // expected RawMessage, "" = don't check
	}{
		{"present string", `{"type":"anthropic","model":"m"}`, "type", true, `"anthropic"`},
		{"present number", `{"count":42}`, "count", true, `42`},
		{"present object", `{"a":{"nested":[1,2]},"b":1}`, "a", true, `{"nested":[1,2]}`},
		{"absent", `{"model":"m"}`, "type", false, ""},
		{"duplicate keys last wins", `{"type":"first","type":"second"}`, "type", true, `"second"`},
		{"unicode escapes in key", `{"t\u7931pe":"v"}`, "t\u7931pe", true, `"v"`},
		{"whitespace tolerance", "{  \"type\" : \"v\"  }", "type", true, `"v"`},
		{"top-level array", `[{"type":"x"}]`, "type", false, ""},
		{"top-level scalar", `"type"`, "type", false, ""},
		{"malformed", `{"type":`, "type", false, ""},
		{"empty", ``, "type", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, found := ExtractTopLevelField([]byte(tt.body), tt.field)
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if tt.wantValue != "" && strings.TrimSpace(string(raw)) != tt.wantValue {
				t.Errorf("value = %s, want %s", raw, tt.wantValue)
			}
		})
	}
}

// FuzzExtractTopLevelField fuzzes the scanner against json.Unmarshal as the
// oracle: field presence must agree with map unmarshaling for every input.
func FuzzExtractTopLevelField(f *testing.F) {
	f.Add([]byte(`{"type":"anthropic","model":"m","usage":{"in":1}}`))
	f.Add([]byte(`{"type":"first","type":"second"}`))
	f.Add([]byte(`{"a":{"type":"nested"}}`))
	f.Add([]byte(`[{"type":"x"}]`))
	f.Add([]byte(`{"type":`))
	f.Add([]byte(``))
	f.Fuzz(func(t *testing.T, body []byte) {
		_, fastFound := ExtractTopLevelField(body, "type")

		var m map[string]interface{}
		err := json.Unmarshal(body, &m)
		_, oracleFound := m["type"]

		if fastFound != (err == nil && oracleFound) {
			t.Fatalf("presence mismatch on %q: scanner=%v oracle=%v (err=%v)",
				body, fastFound, oracleFound, err)
		}
	})
}

// TestExtractTopLevelField_NumberEdgeCases pins RFC 8259 number rules the
// fuzzer coverage exercise validated: leading zeros and malformed numbers
// reject, matching json.Unmarshal.
func TestExtractTopLevelField_NumberEdgeCases(t *testing.T) {
	for _, body := range []string{
		`{"type":01}`,
		`{"type":-01}`,
		`{"type":1.}`,
		`{"type":1e}`,
		`{"type":.5}`,
		`{"type":+1}`,
	} {
		if _, found := ExtractTopLevelField([]byte(body), "type"); found {
			t.Errorf("%s: malformed number must not be found", body)
		}
	}
	for _, body := range []string{
		`{"type":0}`,
		`{"type":-0}`,
		`{"type":1e5}`,
		`{"type":10.25}`,
	} {
		raw, found := ExtractTopLevelField([]byte(body), "type")
		if !found {
			t.Errorf("%s: valid number must be found", body)
			continue
		}
		var v interface{}
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Errorf("%s: captured value must re-parse: %v", body, err)
		}
	}
}
