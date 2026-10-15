package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestInputSchemaPreservesEveryKeyword pins that the tools/list decode is no
// longer lossy (NENYA-172): composition keywords, items, patternProperties, an
// object-valued additionalProperties, and constraints all survive into AsMap.
func TestInputSchemaPreservesEveryKeyword(t *testing.T) {
	raw := `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"properties": {"a": {"$ref": "#/$defs/A"}},
		"items": {"type": "string"},
		"oneOf": [{"required": ["a"]}],
		"additionalProperties": {"type": "string"},
		"$defs": {"A": {"type": "integer", "minimum": 1}},
		"minProperties": 1
	}`
	var schema InputSchema
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	asMap := schema.AsMap()
	for _, key := range []string{"$schema", "properties", "items", "oneOf", "additionalProperties", "$defs", "minProperties"} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("keyword %q was dropped by decoding: %v", key, asMap)
		}
	}
	if _, ok := asMap["additionalProperties"].(map[string]any); !ok {
		t.Errorf("an object-valued additionalProperties must survive: %T", asMap["additionalProperties"])
	}
}

// TestInputSchemaAsMapFromTypedFields pins the fallback for a schema built in
// code (not decoded) and the nil case.
func TestInputSchemaAsMapFromTypedFields(t *testing.T) {
	if got := (InputSchema{}).AsMap(); got != nil {
		t.Fatalf("empty schema AsMap = %v, want nil", got)
	}
	built := InputSchema{Type: "object", Properties: map[string]any{"a": map[string]any{"type": "string"}}, Required: []string{"a"}}
	asMap := built.AsMap()
	if asMap["type"] != "object" || asMap["properties"] == nil {
		t.Fatalf("typed fallback lost fields: %v", asMap)
	}
}

// TestToolNameHygiene pins the NENYA-172 naming rules: charset sanitization, a
// deterministic length cap that cannot collide after truncation, collision
// disambiguation across servers, and stability across a reload.
func TestToolNameHygiene(t *testing.T) {
	long := strings.Repeat("tool-", 40)
	tools := []Tool{{Name: long}, {Name: "weird name/with:chars"}, {Name: "plain"}}

	reg := NewToolRegistry()
	reg.Register("server", tools)
	routes := reg.AllRoutes()
	if len(routes) != 3 {
		t.Fatalf("routes = %d, want 3", len(routes))
	}
	for _, route := range routes {
		if len(route.OpenAIToolName) > 64 {
			t.Errorf("tool name %q exceeds the provider cap", route.OpenAIToolName)
		}
		for _, r := range route.OpenAIToolName {
			valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
			if !valid {
				t.Errorf("tool name %q carries an invalid character %q", route.OpenAIToolName, r)
			}
		}
		if _, ok := reg.Lookup(route.OpenAIToolName); !ok {
			t.Errorf("route %q is not resolvable through the registry", route.OpenAIToolName)
		}
	}

	// A reload of the same server keeps the same names (no request-prefix churn).
	before := map[string]bool{}
	for _, route := range reg.AllRoutes() {
		before[route.OpenAIToolName] = true
	}
	reg.Register("server", tools)
	after := map[string]bool{}
	for _, route := range reg.AllRoutes() {
		after[route.OpenAIToolName] = true
	}
	if len(before) != len(after) {
		t.Fatalf("reload changed the route count: %d -> %d", len(before), len(after))
	}
	for name := range before {
		if !after[name] {
			t.Fatalf("reload renamed %q", name)
		}
	}
}

// TestToolNameCollisionDisambiguated pins that two different (server, tool)
// pairs whose namespaced names collide after SANITIZATION get distinct registry
// keys AND distinct advertised names (the advertised name is what the model
// calls back, so a divergence would make the second server's tool unreachable).
func TestToolNameCollisionDisambiguated(t *testing.T) {
	reg := NewToolRegistry()
	// "a.b__t" and "a_b__t" both sanitize to "a_b__t".
	reg.Register("a.b", []Tool{{Name: "t"}})
	reg.Register("a_b", []Tool{{Name: "t"}})

	routes := reg.AllRoutes()
	if len(routes) != 2 {
		t.Fatalf("routes = %d, want 2 distinct entries", len(routes))
	}
	names := map[string]bool{}
	for _, route := range routes {
		names[route.OpenAIToolName] = true
	}
	if len(names) != 2 {
		t.Fatalf("colliding names were not disambiguated: %v", names)
	}

	// The advertised name must be the registry key for the same pair.
	advertised := map[string]bool{}
	for _, server := range []string{"a.b", "a_b"} {
		for _, tool := range reg.OpenAITools(server, []Tool{{Name: "t"}}) {
			fn := tool["function"].(map[string]any)
			name, _ := fn["name"].(string)
			advertised[name] = true
			route, ok := reg.Lookup(name)
			if !ok {
				t.Fatalf("advertised name %q is not in the registry", name)
			}
			if route.ServerName != server {
				t.Fatalf("advertised name %q routes to server %q, want %q", name, route.ServerName, server)
			}
		}
	}
	if len(advertised) != 2 {
		t.Fatalf("the advertised names collide: %v", advertised)
	}
}

// TestParameterlessToolGetsValidSchema pins the upstream parameterless-tool
// fallback: an empty schema must not reach a provider as `{"type":""}`.
func TestParameterlessToolGetsValidSchema(t *testing.T) {
	openaiTools := openAIToolsForTest(t, "server", []Tool{{Name: "ping"}})
	fn := openaiTools[0]["function"].(map[string]any)
	params, ok := fn["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters type = %T", fn["parameters"])
	}
	if params["type"] != "object" {
		t.Fatalf("parameterless tool type = %v, want object", params["type"])
	}
	if _, ok := params["properties"].(map[string]any); !ok {
		t.Fatalf("parameterless tool properties = %T, want an object", params["properties"])
	}
}

// TestMCPToolSchemaSanitized pins that the sanitizer runs on the injected schema
// (dialect keywords and $refs never reach a provider from the MCP path).
func TestMCPToolSchemaSanitized(t *testing.T) {
	tools := []Tool{{
		Name: "fetch",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]any{
				"url": map[string]any{"$ref": "#/$defs/URL"},
			},
			raw: map[string]any{
				"$schema":    "https://json-schema.org/draft/2020-12/schema",
				"type":       "OBJECT",
				"properties": map[string]any{"url": map[string]any{"$ref": "#/$defs/URL"}},
				"$defs":      map[string]any{"URL": map[string]any{"type": "STRING"}},
			},
		},
	}}
	fn := openAIToolsForTest(t, "server", tools)[0]["function"].(map[string]any)
	encoded, err := json.Marshal(fn["parameters"])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(encoded)
	for _, banned := range []string{"$schema", "$ref", "$defs", "OBJECT"} {
		if strings.Contains(body, banned) {
			t.Errorf("injected schema still carries %q: %s", banned, body)
		}
	}
}

// openAIToolsForTest registers the tools and returns the registry's advertised
// declarations, so the tests exercise the same path as the proxy.
func openAIToolsForTest(t *testing.T, serverName string, tools []Tool) []map[string]any {
	t.Helper()
	reg := NewToolRegistry()
	if len(tools) > 0 {
		reg.Register(serverName, tools)
	}
	return reg.OpenAITools(serverName, tools)
}
