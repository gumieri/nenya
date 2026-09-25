package mcp

import (
	"encoding/json"
	"strings"
	"sync"
)

type ToolRouting struct {
	OpenAIToolName string
	ServerName     string
	MCPToolName    string
	// Schema is the tool's declared InputSchema rendered as generic
	// JSON (nil-safe: tools without a schema validate as pass-through).
	// Snapshotted at registration; the registry is rebuilt on reload.
	Schema any
}

type ToolRegistry struct {
	mu     sync.RWMutex
	routes map[string]ToolRouting
}

// schemaAsAny renders a typed InputSchema as generic decoded JSON so
// the argument validator can walk nested properties/items uniformly.
func schemaAsAny(schema InputSchema) any {
	if schema.Type == "" && len(schema.Properties) == 0 && len(schema.Required) == 0 {
		return nil
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil
	}
	return out
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		routes: make(map[string]ToolRouting),
	}
}

func (r *ToolRegistry) Register(serverName string, tools []Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, tool := range tools {
		openaiName := buildOpenAIToolName(serverName, tool.Name)
		r.routes[openaiName] = ToolRouting{
			OpenAIToolName: openaiName,
			ServerName:     serverName,
			MCPToolName:    tool.Name,
			Schema:         schemaAsAny(tool.InputSchema),
		}
	}
}

func (r *ToolRegistry) Lookup(openaiName string) (ToolRouting, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	route, ok := r.routes[openaiName]
	return route, ok
}

func (r *ToolRegistry) IsMCPTool(openaiName string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.routes[openaiName]
	return ok
}

func (r *ToolRegistry) AllRoutes() []ToolRouting {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]ToolRouting, 0, len(r.routes))
	for _, route := range r.routes {
		result = append(result, route)
	}
	return result
}

func (r *ToolRegistry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes = make(map[string]ToolRouting)
}

func buildOpenAIToolName(serverName, toolName string) string {
	return serverName + "__" + toolName
}

func MCPToolsToOpenAI(serverName string, tools []Tool) []map[string]any {
	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		openaiTool := map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        buildOpenAIToolName(serverName, tool.Name),
				"description": tool.Description,
				"parameters":  tool.InputSchema,
			},
		}
		result = append(result, openaiTool)
	}
	return result
}

func ParseMCPCall(openaiToolName string) (serverName string, mcpToolName string, ok bool) {
	idx := strings.Index(openaiToolName, "__")
	if idx <= 0 || idx+2 >= len(openaiToolName) {
		return "", "", false
	}
	return openaiToolName[:idx], openaiToolName[idx+2:], true
}

func BuildToolResultMessage(toolCallID string, result *CallToolResult, isError bool) map[string]any {
	content := result.Text()
	if isError || result.IsError {
		content = "[MCP Error] " + content
	}

	return map[string]any{
		"role":         "tool",
		"tool_call_id": toolCallID,
		"content":      content,
	}
}
