package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// contractSchemaDir is the published contract schema location, relative to the
// cmd/nenya package directory.
var contractSchemaDir = filepath.Join("..", "..", "docs", "contract")

// TestContractSchemaExamples validates the published example fixtures against
// their JSON Schemas. The fixtures are the CONTRACT.md Appendix A examples; a
// schema edit that no longer accepts the documented shape, or a fixture that
// drifts from the schema, fails here.
func TestContractSchemaExamples(t *testing.T) {
	for _, name := range []string{"version", "paths", "describe"} {
		t.Run(name, func(t *testing.T) {
			schema := loadJSON(t, filepath.Join(contractSchemaDir, name+".schema.json"))
			doc := loadJSON(t, filepath.Join(contractSchemaDir, "examples", name+".json"))
			for _, err := range schemaErrors(schema, doc, name) {
				t.Error(err)
			}
		})
	}
}

// TestVersionOutputMatchesContractSchema asserts that the live `version --json`
// output conforms to the published schema, so the schema cannot silently drift
// from the command it documents.
func TestVersionOutputMatchesContractSchema(t *testing.T) {
	var buf bytes.Buffer
	handled, err := handleVersion(&buf, []string{"version", "--json"})
	if err != nil {
		t.Fatalf("handleVersion: %v", err)
	}
	if !handled {
		t.Fatal("expected version --json to be handled")
	}

	schema := loadJSON(t, filepath.Join(contractSchemaDir, "version.schema.json"))
	doc := decodeJSON(t, buf.Bytes())
	for _, err := range schemaErrors(schema, doc, "version") {
		t.Error(err)
	}
}

// TestPathsOutputMatchesContractSchema asserts that the live `paths --json`
// output conforms to the published schema.
func TestPathsOutputMatchesContractSchema(t *testing.T) {
	t.Setenv("NENYA_CONFIG_DIR", t.TempDir())
	t.Setenv("NENYA_CONFIG_FILE", "")
	t.Setenv("NENYA_SECRETS_DIR", "")

	var buf, errBuf bytes.Buffer
	handled, err := handlePaths(&buf, &errBuf, []string{"paths", "--json"})
	if err != nil {
		t.Fatalf("handlePaths: %v", err)
	}
	if !handled {
		t.Fatal("expected paths --json to be handled")
	}

	schema := loadJSON(t, filepath.Join(contractSchemaDir, "paths.schema.json"))
	doc := decodeJSON(t, buf.Bytes())
	for _, err := range schemaErrors(schema, doc, "paths") {
		t.Error(err)
	}
}

// TestDescribeOutputMatchesContractSchema asserts that the live `describe
// --json` output conforms to the published schema.
func TestDescribeOutputMatchesContractSchema(t *testing.T) {
	setupDescribeEnv(t, "{}")

	var buf, errBuf bytes.Buffer
	handled, err := handleDescribe(&buf, &errBuf, []string{"describe", "--json"})
	if err != nil {
		t.Fatalf("handleDescribe: %v", err)
	}
	if !handled {
		t.Fatal("expected describe --json to be handled")
	}

	schema := loadJSON(t, filepath.Join(contractSchemaDir, "describe.schema.json"))
	doc := decodeJSON(t, buf.Bytes())
	for _, err := range schemaErrors(schema, doc, "describe") {
		t.Error(err)
	}
}

// TestContractSchemaValidatorRejectsDrift proves the validator is not vacuous:
// a document missing a required field, carrying a wrong type, or adding an
// unexpected property must be rejected. Without this, a broken validator would
// make the conformance tests above pass trivially.
func TestContractSchemaValidatorRejectsDrift(t *testing.T) {
	schema := loadJSON(t, filepath.Join(contractSchemaDir, "version.schema.json"))

	cases := map[string]any{
		"missing_required": map[string]any{
			"version": "0.15.0", "commit": "abc", "build_time": "t",
		},
		"wrong_type": map[string]any{
			"version": 15, "commit": "abc", "build_time": "t", "contract_version": 1,
		},
		"below_minimum": map[string]any{
			"version": "0.15.0", "commit": "abc", "build_time": "t", "contract_version": 0,
		},
		"unexpected_property": map[string]any{
			"version": "0.15.0", "commit": "abc", "build_time": "t", "contract_version": 1,
			"extra": true,
		},
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if errs := schemaErrors(schema, doc, name); len(errs) == 0 {
				t.Errorf("expected a validation error for %s, got none", name)
			}
		})
	}
}

// loadJSON reads and decodes a JSON document, failing the test on any error.
func loadJSON(t *testing.T, path string) any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return decodeJSON(t, data)
}

// decodeJSON decodes raw JSON into the generic map/slice/scalar representation
// used by the schema validator.
func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	return doc
}

// schemaErrors checks doc against the JSON Schema subset used by the contract
// schemas: type (name or list of names), enum, minimum, required, properties,
// additionalProperties (false forbids extras) and items. Unknown keywords are
// ignored. It returns every violation found. It is deliberately tiny and
// stdlib-only; the nenya gateway takes no third-party dependency.
func schemaErrors(schema, doc any, path string) []string {
	s, ok := schema.(map[string]any)
	if !ok {
		return []string{fmt.Sprintf("%s: schema node is %T, want object", path, schema)}
	}

	var errs []string
	if typ, present := s["type"]; present && !valueMatchesType(typ, doc) {
		errs = append(errs, fmt.Sprintf("%s: value %v does not match type %v", path, doc, typ))
	}
	if enum, ok := s["enum"].([]any); ok && !valueInEnum(enum, doc) {
		errs = append(errs, fmt.Sprintf("%s: %v not one of %v", path, doc, enum))
	}
	if min, ok := s["minimum"].(float64); ok {
		if n, ok := doc.(float64); ok && n < min {
			errs = append(errs, fmt.Sprintf("%s: %v is below minimum %v", path, doc, min))
		}
	}

	switch d := doc.(type) {
	case map[string]any:
		errs = append(errs, objectErrors(s, d, path)...)
	case []any:
		items, ok := s["items"]
		if !ok {
			return errs
		}
		for i, item := range d {
			errs = append(errs, schemaErrors(items, item, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return errs
}

// objectErrors applies object-specific keywords (required,
// additionalProperties, properties) to an object document.
func objectErrors(schema, doc map[string]any, path string) []string {
	var errs []string
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			name, _ := r.(string)
			if _, present := doc[name]; !present {
				errs = append(errs, fmt.Sprintf("%s: missing required property %q", path, name))
			}
		}
	}

	props, _ := schema["properties"].(map[string]any)

	if allowExtra, ok := schema["additionalProperties"].(bool); ok && !allowExtra {
		for k := range doc {
			if _, known := props[k]; !known {
				errs = append(errs, fmt.Sprintf("%s: unexpected property %q", path, k))
			}
		}
	}

	for key, value := range doc {
		sub, ok := props[key]
		if !ok {
			continue
		}
		errs = append(errs, schemaErrors(sub, value, path+"."+key)...)
	}
	return errs
}

// valueMatchesType reports whether doc satisfies a schema "type" keyword, which
// is either a single type name or a list of accepted names.
func valueMatchesType(typ, doc any) bool {
	switch tt := typ.(type) {
	case string:
		return matchesSingleType(tt, doc)
	case []any:
		for _, candidate := range tt {
			if name, ok := candidate.(string); ok && matchesSingleType(name, doc) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

// matchesSingleType reports whether doc is a JSON value of the named type.
func matchesSingleType(name string, doc any) bool {
	switch name {
	case "object":
		_, ok := doc.(map[string]any)
		return ok
	case "array":
		_, ok := doc.([]any)
		return ok
	case "string":
		_, ok := doc.(string)
		return ok
	case "boolean":
		_, ok := doc.(bool)
		return ok
	case "number":
		_, ok := doc.(float64)
		return ok
	case "integer":
		f, ok := doc.(float64)
		return ok && f == float64(int64(f))
	case "null":
		return doc == nil
	default:
		return true
	}
}

// valueInEnum reports whether doc equals one of the enum values.
func valueInEnum(enum []any, doc any) bool {
	for _, candidate := range enum {
		if candidate == doc {
			return true
		}
	}
	return false
}
