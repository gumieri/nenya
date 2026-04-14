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
		{"mempalace", "mempalace_search", "mempalace__mempalace_search"},
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

	openaiTools := MCPToolsToOpenAI("mempalace", tools)

	if len(openaiTools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(openaiTools))
	}

	tool0 := openaiTools[0]
	if tool0["type"] != "function" {
		t.Fatalf("tool type = %v, want function", tool0["type"])
	}

	fn0 := tool0["function"].(map[string]any)
	if fn0["name"] != "mempalace__search" {
		t.Fatalf("function name = %q, want %q", fn0["name"], "mempalace__search")
	}
	if fn0["description"] != "Search the palace" {
		t.Fatalf("description = %q, want %q", fn0["description"], "Search the palace")
	}

	params, ok := fn0["parameters"].(InputSchema)
	if !ok {
		t.Fatal("parameters is not InputSchema")
	}
	if params.Type != "object" {
		t.Fatalf("parameters.type = %q, want object", params.Type)
	}
	if len(params.Required) != 1 || params.Required[0] != "query" {
		t.Fatalf("parameters.required = %v, want [query]", params.Required)
	}

	tool1 := openaiTools[1]
	fn1 := tool1["function"].(map[string]any)
	if fn1["name"] != "mempalace__status" {
		t.Fatalf("function name = %q, want %q", fn1["name"], "mempalace__status")
	}
}

func TestMCPToolsToOpenAI_Empty(t *testing.T) {
	tools := MCPToolsToOpenAI("server", nil)
	if len(tools) != 0 {
		t.Fatalf("expected 0 tools, got %d", len(tools))
	}

	tools = MCPToolsToOpenAI("server", []Tool{})
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
		{"mempalace__mempalace_search", "mempalace", "mempalace_search", true},
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

	reg.Register("mempalace", tools)

	route, ok := reg.Lookup("mempalace__search")
	if !ok {
		t.Fatal("expected mempalace__search to be registered")
	}
	if route.ServerName != "mempalace" {
		t.Fatalf("ServerName = %q, want mempalace", route.ServerName)
	}
	if route.MCPToolName != "search" {
		t.Fatalf("MCPToolName = %q, want search", route.MCPToolName)
	}

	route, ok = reg.Lookup("mempalace__status")
	if !ok {
		t.Fatal("expected mempalace__status to be registered")
	}

	_, ok = reg.Lookup("other__search")
	if ok {
		t.Fatal("expected other__search to not be registered")
	}
}

func TestToolRegistry_MultipleServers(t *testing.T) {
	reg := NewToolRegistry()

	reg.Register("mempalace", []Tool{
		{Name: "search", Description: "Search palace"},
	})
	reg.Register("files", []Tool{
		{Name: "read", Description: "Read file"},
	})

	route1, ok := reg.Lookup("mempalace__search")
	if !ok {
		t.Fatal("expected mempalace__search")
	}
	if route1.ServerName != "mempalace" {
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
	reg.Register("mempalace", []Tool{{Name: "search"}})

	if !reg.IsMCPTool("mempalace__search") {
		t.Fatal("expected IsMCPTool true for mempalace__search")
	}
	if reg.IsMCPTool("code_edit") {
		t.Fatal("expected IsMCPTool false for code_edit")
	}
	if reg.IsMCPTool("mempalace__nonexistent") {
		t.Fatal("expected IsMCPTool false for unregistered tool")
	}
}

func TestToolRegistry_AllRoutes(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register("mempalace", []Tool{
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
	reg.Register("mempalace", []Tool{{Name: "search"}})

	reg.Clear()

	_, ok := reg.Lookup("mempalace__search")
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

	openaiTools := MCPToolsToOpenAI("test", tools)
	fn := openaiTools[0]["function"].(map[string]any)
	params := fn["parameters"].(InputSchema)

	if params.AdditionalProperties {
		t.Fatal("AdditionalProperties should be preserved as false")
	}
	if len(params.Properties) != 4 {
		t.Fatalf("expected 4 properties, got %d", len(params.Properties))
	}
	if _, ok := params.Properties["tags"]; !ok {
		t.Fatal("expected tags property")
	}
}
