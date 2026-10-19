package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/nenya/internal/jsonschema"
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
	// byPair maps a (server, tool) pair to its registry key, so the advertised
	// function name and the dispatch key are always the same string.
	byPair map[string]string
}

// schemaAsAny renders a typed InputSchema as generic decoded JSON so the
// argument validator can walk nested properties/items uniformly. Every keyword
// is preserved (NENYA-172), so the declaration reaches the validator intact;
// the validator itself still enforces only its documented subset
// (type/enum/required/properties/items) and ignores the rest.
func schemaAsAny(schema InputSchema) any {
	return schema.AsMap()
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		routes: make(map[string]ToolRouting),
		byPair: make(map[string]string),
	}
}

func (r *ToolRegistry) Register(serverName string, tools []Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, tool := range tools {
		openaiName := r.uniqueToolName(serverName, tool.Name)
		r.routes[openaiName] = ToolRouting{
			OpenAIToolName: openaiName,
			ServerName:     serverName,
			MCPToolName:    tool.Name,
			Schema:         schemaAsAny(tool.InputSchema),
		}
		r.byPair[pairKey(serverName, tool.Name)] = openaiName
	}
}

// OpenAITools builds the OpenAI-shaped declarations for one server from the
// registry's routes, so the name advertised to the model is exactly the name the
// registry dispatches on: a namespacing collision can no longer make two servers
// advertise the same function name (NENYA-172).
func (r *ToolRegistry) OpenAITools(serverName string, tools []Tool) []map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		name, ok := r.byPair[pairKey(serverName, tool.Name)]
		if !ok {
			name = buildOpenAIToolName(serverName, tool.Name)
		}
		result = append(result, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": tool.Description,
				"parameters":  jsonschema.Sanitize(tool.InputSchema.AsMap()),
			},
		})
	}
	return result
}

// AdvertisedName returns the function name the registry advertises for one
// (server, tool) pair: the registered route name when the pair is registered,
// else the same deterministic fallback OpenAITools uses — so a caller deriving
// the injected tool set sees exactly the names the model was handed.
func (r *ToolRegistry) AdvertisedName(serverName, toolName string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name, ok := r.byPair[pairKey(serverName, toolName)]; ok {
		return name
	}
	return buildOpenAIToolName(serverName, toolName)
}

// pairKey identifies a (server, tool) pair in the byPair index.
func pairKey(serverName, toolName string) string {
	return serverName + "\x00" + toolName
}

// uniqueToolName returns a registry key for one (server, tool) pair that is
// stable across reloads and unique across servers: the namespaced name is
// charset-sanitized and length-capped, and a name already claimed by a DIFFERENT
// pair gets a deterministic digest suffix (NENYA-172). Re-registering the same
// pair keeps its name, so a reload never churns the request prefix.
func (r *ToolRegistry) uniqueToolName(serverName, toolName string) string {
	base := buildOpenAIToolName(serverName, toolName)
	existing, taken := r.routes[base]
	if !taken || (existing.ServerName == serverName && existing.MCPToolName == toolName) {
		return base
	}
	// Deterministic disambiguation, with a counter so an (astronomically
	// unlikely) digest collision still terminates in a free name.
	sum := sha256.Sum256([]byte(pairKey(serverName, toolName)))
	digest := hex.EncodeToString(sum[:])[:8]
	for attempt := 0; ; attempt++ {
		candidate := jsonschema.CapToolName(base + "-" + digest)
		if attempt > 0 {
			candidate = jsonschema.CapToolName(fmt.Sprintf("%s-%s%d", base, digest, attempt))
		}
		claimed, ok := r.routes[candidate]
		if !ok || (claimed.ServerName == serverName && claimed.MCPToolName == toolName) {
			return candidate
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
	r.byPair = make(map[string]string)
}

// buildOpenAIToolName namespaces a tool by its server. Both parts are sanitized
// to the provider function-name charset and the result is length-capped with a
// deterministic digest suffix, so a hostile or oversized MCP tool name cannot
// produce a provider 400 (NENYA-172).
func buildOpenAIToolName(serverName, toolName string) string {
	return jsonschema.CapToolName(jsonschema.SanitizeToolName(serverName) + "__" + jsonschema.SanitizeToolName(toolName))
}

// ParseMCPCall splits a namespaced tool name on the FIRST "__" separator. It is
// a convenience for callers that only have the name string; the registry is the
// source of truth for dispatch (a sanitized or disambiguated name need not
// split back into the original pair).
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
