package auth

import (
	"net/http"
	"slices"
	"strings"

	"github.com/nenya/config"
)

// Role represents a user role with specific permissions.
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleUser     Role = "user"
	RoleReadOnly Role = "read-only"
)

// Permission represents a specific action or resource access.
type Permission string

const (
	PermissionChat    Permission = "chat"
	PermissionModels  Permission = "models"
	PermissionEmbed   Permission = "embed"
	PermissionMetrics Permission = "metrics"
	PermissionAdmin   Permission = "admin"
)

// RolePermissions maps roles to the set of permissions they grant.
var RolePermissions = map[Role][]Permission{
	RoleAdmin:    {PermissionChat, PermissionModels, PermissionEmbed, PermissionMetrics, PermissionAdmin},
	RoleUser:     {PermissionChat, PermissionModels, PermissionEmbed},
	RoleReadOnly: {PermissionModels, PermissionMetrics},
}

// HasPermission checks if a role grants the specified permission.
func HasPermission(role Role, perm Permission) bool {
	perms, ok := RolePermissions[role]
	if !ok {
		return false
	}
	for _, p := range perms {
		if p == perm {
			return true
		}
	}
	return false
}

// rolesRecognized reports whether every role on the key is a known role and
// at least one is present. Load validation rejects unknown/absent roles; this
// runtime check keeps a key that bypassed validation from falling through to
// the permissive branches of AuthorizeAgent/AuthorizeEndpoint (NENYA-196,
// fail closed).
func rolesRecognized(apiKey *config.ApiKey) bool {
	if apiKey == nil || len(apiKey.Roles) == 0 {
		return false
	}
	for _, roleStr := range apiKey.Roles {
		if _, ok := RolePermissions[Role(roleStr)]; !ok {
			return false
		}
	}
	return true
}

// AuthorizeAgent checks if the key is allowed to access the given agent.
// Admin keys have unrestricted access to all agents.
// An empty AllowedAgents list grants access to all agents.
// Returns false if the key is nil, carries an unrecognized role, or the
// agent is not in the allowed list.
func AuthorizeAgent(apiKey *config.ApiKey, agentName string) bool {
	if apiKey == nil || !rolesRecognized(apiKey) {
		return false
	}
	for _, roleStr := range apiKey.Roles {
		if Role(roleStr) == RoleAdmin {
			return true
		}
	}

	if len(apiKey.AllowedAgents) == 0 {
		return true
	}

	for _, allowed := range apiKey.AllowedAgents {
		if allowed == agentName {
			return true
		}
	}
	return false
}

// canonicalChatRequest maps the two chat wire routes (/v1/chat/completions
// and /v1/messages) onto one logical endpoint for allowed_endpoints matching:
// the canonical "POST /v1/chat/completions" entry authorizes BOTH routes,
// while an explicit "POST /v1/messages" entry authorizes only /v1/messages
// (see matchesAllowedEndpoints). Returns ("", false) for non-POST methods
// and all other paths.
func canonicalChatRequest(method, path string) (string, bool) {
	if method != "POST" {
		return "", false
	}
	switch path {
	case "/v1/chat/completions", "/v1/messages":
		return "POST /v1/chat/completions", true
	default:
		return "", false
	}
}

// matchesAllowedEndpoints reports whether any entry in allowed grants
// (method, path). The two chat wire routes are one logical endpoint: the
// canonical "POST /v1/chat/completions" entry also authorizes /v1/messages.
func matchesAllowedEndpoints(allowed []string, method, path string) bool {
	requested := method + " " + path
	if slices.Contains(allowed, requested) {
		return true
	}
	if canonical, ok := canonicalChatRequest(method, path); ok && canonical != requested {
		return slices.Contains(allowed, canonical)
	}
	return false
}

// AuthorizeMCPTool reports whether the key may invoke an MCP tool
// (NENYA-183). The effective grant is the intersection of the key's
// allowed_tools and the tools the agent's MCP configuration exposes; the
// agent side is enforced separately (tools outside the agent's servers are
// never injected or dispatched).
//
// Semantics: an empty grant allows every tool the agent exposes (backward
// compatible), an admin key bypasses the cap, and a nil key or one carrying an
// unrecognized role is denied (fail closed). Entries match the namespaced tool
// name exactly ("server__tool"), a server wildcard ("server__*"), or the bare
// tool name on any server ("tool").
func AuthorizeMCPTool(apiKey *config.ApiKey, serverName, toolName string) bool {
	if apiKey == nil || !rolesRecognized(apiKey) {
		return false
	}
	for _, roleStr := range apiKey.Roles {
		if Role(roleStr) == RoleAdmin {
			return true
		}
	}
	if len(apiKey.AllowedTools) == 0 {
		return true
	}
	namespaced := serverName + "__" + toolName
	wildcard := serverName + "__*"
	for _, allowed := range apiKey.AllowedTools {
		switch {
		case allowed == namespaced || allowed == wildcard:
			return true
		case !strings.Contains(allowed, "__") && allowed == toolName:
			return true
		}
	}
	return false
}

// MCPGrantTouchesServer reports whether a key's allowed_tools grant could
// cover any tool of a server (NENYA-183). It is a conservative reachability
// test used to decide whether a server's health matters to this key: an empty
// grant reaches every server, a namespaced entry ("server__tool" or
// "server__*") reaches its own server only, and a bare entry (matched against
// the tool's own name on any server) could reach any server, so it counts as
// touching every server. An admin key reaches every server. Note the
// asymmetry with AuthorizeMCPTool: a nil key reaches nothing here (so a
// server's health does not fail a request that could not use it anyway), while
// AuthorizeMCPTool denies it outright.
func MCPGrantTouchesServer(apiKey *config.ApiKey, serverName string) bool {
	if apiKey == nil || !rolesRecognized(apiKey) {
		return false
	}
	for _, roleStr := range apiKey.Roles {
		if Role(roleStr) == RoleAdmin {
			return true
		}
	}
	if len(apiKey.AllowedTools) == 0 {
		return true
	}
	prefix := serverName + "__"
	for _, allowed := range apiKey.AllowedTools {
		if !strings.Contains(allowed, "__") {
			// A bare entry matches the tool name on any server.
			return true
		}
		if strings.HasPrefix(allowed, prefix) {
			return true
		}
	}
	return false
}

// AuthorizeEndpoint checks if the key is allowed to access the given HTTP endpoint.
// Admin keys bypass endpoint restrictions.
// Custom allowed_endpoints override default role-based permissions. The two
// chat wire routes are treated as one logical endpoint: a
// "POST /v1/chat/completions" entry also authorizes /v1/messages (an explicit
// "POST /v1/messages" entry authorizes only /v1/messages).
// read-only role restricts access to GET requests only.
// Returns false if the key is nil, carries an unrecognized role, or the
// endpoint is not authorized.
func AuthorizeEndpoint(apiKey *config.ApiKey, method, path string) bool {
	if apiKey == nil || !rolesRecognized(apiKey) {
		return false
	}
	for _, roleStr := range apiKey.Roles {
		if Role(roleStr) == RoleAdmin {
			return true
		}
	}

	if len(apiKey.AllowedEndpoints) > 0 {
		return matchesAllowedEndpoints(apiKey.AllowedEndpoints, method, path)
	}

	for _, roleStr := range apiKey.Roles {
		if Role(roleStr) == RoleReadOnly {
			// Read-only is a monitoring role scoped to the documented GET
			// surfaces (NENYA-131): unrestricted GET access would also
			// authorize /proxy/* passthrough reads and /debug/pprof.
			if method != http.MethodGet {
				return false
			}
			switch path {
			case "/healthz", "/statsz", "/metrics", "/v1/models":
			default:
				return false
			}
		}
	}
	return true
}
