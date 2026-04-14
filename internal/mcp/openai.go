package mcp

import (
	"fmt"
	"strings"
	"sync"
)

type ToolRouting struct {
	OpenAIToolName string
	ServerName     string
	MCPToolName    string
}

type ToolRegistry struct {
	mu     sync.RWMutex
	routes map[string]ToolRouting
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
		content = fmt.Sprintf("[MCP Error] %s", content)
	}

	return map[string]any{
		"role":         "tool",
		"tool_call_id": toolCallID,
		"content":      content,
	}
}
