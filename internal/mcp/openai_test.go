package mcp

import (
	"testing"
)

func TestBuildOpenAIToolName(t *testing.T) {
	tests := []struct {
		server   string
		tool     string
		expected string
	}{
		{"memory", "memory_search", "memory__memory_search"},
		{"mcp1", "tool_a", "mcp1__tool_a"},
		{"my-server", "my_tool", "my-server__my_tool"},
	}

	for _, tt := range tests {
		got := buildOpenAIToolName(tt.server, tt.tool)
		if got != tt.expected {
			t.Errorf("buildOpenAIToolName(%q, %q) = %q, want %q", tt.server, tt.tool, got, tt.expected)
		}
	}
}

func TestMCPToolsToOpenAI(t *testing.T) {
	tools := []Tool{
		{
			Name:        "search",
			Description: "Search the palace",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]any{
					"query": map[string]any{"type": "string"},
				},
				Required: []string{"query"},
			},
		},
		{
			Name:        "status",
			Description: "Get status",
			InputSchema: InputSchema{
				Type:       "object",
				Properties: map[string]any{},
			},
		},
	}

	openaiTools := openAIToolsForTest(t, "memory", tools)

	if len(openaiTools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(openaiTools))
	}

	tool0 := openaiTools[0]
	if tool0["type"] != "function" {
		t.Fatalf("tool type = %v, want function", tool0["type"])
	}

	fn0 := tool0["function"].(map[string]any)
	if fn0["name"] != "memory__search" {
		t.Fatalf("function name = %q, want %q", fn0["name"], "memory__search")
	}
	if fn0["description"] != "Search the palace" {
		t.Fatalf("description = %q, want %q", fn0["description"], "Search the palace")
	}

	params, ok := fn0["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters type = %T, want the sanitized schema map", fn0["parameters"])
	}
	if params["type"] != "object" {
		t.Fatalf("parameters.type = %v, want object", params["type"])
	}
	required, _ := params["required"].([]any)
	if len(required) != 1 || required[0] != "query" {
		t.Fatalf("parameters.required = %v, want [query]", params["required"])
	}

	tool1 := openaiTools[1]
	fn1 := tool1["function"].(map[string]any)
	if fn1["name"] != "memory__status" {
		t.Fatalf("function name = %q, want %q", fn1["name"], "memory__status")
	}
}

func TestMCPToolsToOpenAI_Empty(t *testing.T) {
	tools := openAIToolsForTest(t, "server", nil)
	if len(tools) != 0 {
		t.Fatalf("expected 0 tools, got %d", len(tools))
	}

	tools = openAIToolsForTest(t, "server", []Tool{})
	if len(tools) != 0 {
		t.Fatalf("expected 0 tools, got %d", len(tools))
	}
}

func TestParseMCPCall(t *testing.T) {
	tests := []struct {
		input      string
		wantServer string
		wantTool   string
		wantOK     bool
	}{
		{"memory__memory_search", "memory", "memory_search", true},
		{"mcp1__tool_a", "mcp1", "tool_a", true},
		{"my-server__my_tool", "my-server", "my_tool", true},
		{"no_separator", "", "", false},
		{"__only_suffix", "", "", false},
		{"only_prefix__", "", "", false},
		{"_single", "", "", false},
	}

	for _, tt := range tests {
		server, tool, ok := ParseMCPCall(tt.input)
		if server != tt.wantServer || tool != tt.wantTool || ok != tt.wantOK {
			t.Errorf("ParseMCPCall(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.input, server, tool, ok, tt.wantServer, tt.wantTool, tt.wantOK)
		}
	}
}

func TestToolRegistry_Register(t *testing.T) {
	reg := NewToolRegistry()

	tools := []Tool{
		{Name: "search", Description: "Search"},
		{Name: "status", Description: "Status"},
	}

	reg.Register("memory", tools)

	route, ok := reg.Lookup("memory__search")
	if !ok {
		t.Fatal("expected memory__search to be registered")
	}
	if route.ServerName != "memory" {
		t.Fatalf("ServerName = %q, want memory", route.ServerName)
	}
	if route.MCPToolName != "search" {
		t.Fatalf("MCPToolName = %q, want search", route.MCPToolName)
	}

	route, ok = reg.Lookup("memory__status")
	if !ok {
		t.Fatal("expected memory__status to be registered")
	}

	_, ok = reg.Lookup("other__search")
	if ok {
		t.Fatal("expected other__search to not be registered")
	}
}

func TestToolRegistry_MultipleServers(t *testing.T) {
	reg := NewToolRegistry()

	reg.Register("memory", []Tool{
		{Name: "search", Description: "Search palace"},
	})
	reg.Register("files", []Tool{
		{Name: "read", Description: "Read file"},
	})

	route1, ok := reg.Lookup("memory__search")
	if !ok {
		t.Fatal("expected memory__search")
	}
	if route1.ServerName != "memory" {
		t.Fatalf("ServerName = %q", route1.ServerName)
	}

	route2, ok := reg.Lookup("files__read")
	if !ok {
		t.Fatal("expected files__read")
	}
	if route2.ServerName != "files" {
		t.Fatalf("ServerName = %q", route2.ServerName)
	}
}

func TestToolRegistry_IsMCPTool(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register("memory", []Tool{{Name: "search"}})

	if !reg.IsMCPTool("memory__search") {
		t.Fatal("expected IsMCPTool true for memory__search")
	}
	if reg.IsMCPTool("code_edit") {
		t.Fatal("expected IsMCPTool false for code_edit")
	}
	if reg.IsMCPTool("memory__nonexistent") {
		t.Fatal("expected IsMCPTool false for unregistered tool")
	}
}

func TestToolRegistry_AllRoutes(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register("memory", []Tool{
		{Name: "search"},
		{Name: "status"},
	})

	routes := reg.AllRoutes()
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}
}

func TestToolRegistry_Clear(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register("memory", []Tool{{Name: "search"}})

	reg.Clear()

	_, ok := reg.Lookup("memory__search")
	if ok {
		t.Fatal("expected tool to be cleared")
	}
}

func TestBuildToolResultMessage_Success(t *testing.T) {
	result := &CallToolResult{
		Content: []ContentBlock{{Type: "text", Text: "found 3 results"}},
	}

	msg := BuildToolResultMessage("call_123", result, false)

	if msg["role"] != "tool" {
		t.Fatalf("role = %v, want tool", msg["role"])
	}
	if msg["tool_call_id"] != "call_123" {
		t.Fatalf("tool_call_id = %v, want call_123", msg["tool_call_id"])
	}
	if msg["content"] != "found 3 results" {
		t.Fatalf("content = %v, want 'found 3 results'", msg["content"])
	}
}

func TestBuildToolResultMessage_Error(t *testing.T) {
	result := &CallToolResult{
		Content: []ContentBlock{{Type: "text", Text: "tool not found"}},
		IsError: true,
	}

	msg := BuildToolResultMessage("call_456", result, true)

	content, _ := msg["content"].(string)
	if content != "[MCP Error] tool not found" {
		t.Fatalf("content = %q, want '[MCP Error] tool not found'", content)
	}
}

func TestBuildToolResultMessage_IsErrorFlag(t *testing.T) {
	result := &CallToolResult{
		Content: []ContentBlock{{Type: "text", Text: "something went wrong"}},
		IsError: true,
	}

	msg := BuildToolResultMessage("call_789", result, false)

	content, _ := msg["content"].(string)
	if content != "[MCP Error] something went wrong" {
		t.Fatalf("content = %q, want error prefix", content)
	}
}

func TestMCPToolsToOpenAI_SchemaPreserved(t *testing.T) {
	tools := []Tool{
		{
			Name:        "complex_tool",
			Description: "A tool with complex schema",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]any{
					"name":    map[string]any{"type": "string"},
					"age":     map[string]any{"type": "integer"},
					"tags":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"enabled": map[string]any{"type": "boolean"},
				},
				Required:             []string{"name"},
				AdditionalProperties: false,
			},
		},
	}

	openaiTools := openAIToolsForTest(t, "test", tools)
	fn := openaiTools[0]["function"].(map[string]any)
	params, ok := fn["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters type = %T, want a sanitized schema map", fn["parameters"])
	}

	// NENYA-172: the injected schema is the sanitized generic map, so
	// additionalProperties survives as the decoded value and every constraint
	// is preserved.
	if got, ok := params["additionalProperties"].(bool); !ok || got {
		t.Fatalf("additionalProperties = %v, want the preserved false", params["additionalProperties"])
	}
	props, _ := params["properties"].(map[string]any)
	if len(props) != 4 {
		t.Fatalf("expected 4 properties, got %d", len(props))
	}
	if _, ok := props["tags"]; !ok {
		t.Fatal("expected tags property")
	}
	if params["type"] != "object" {
		t.Fatalf("type = %v, want object", params["type"])
	}
}
