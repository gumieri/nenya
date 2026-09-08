package auth

import (
	"slices"

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

// AuthorizeAgent checks if the key is allowed to access the given agent.
// Admin keys have unrestricted access to all agents.
// An empty AllowedAgents list grants access to all agents.
// Returns false if the key is nil or the agent is not in the allowed list.
func AuthorizeAgent(apiKey *config.ApiKey, agentName string) bool {
	if apiKey == nil {
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

// AuthorizeEndpoint checks if the key is allowed to access the given HTTP endpoint.
// Admin keys bypass endpoint restrictions.
// Custom allowed_endpoints override default role-based permissions. The two
// chat wire routes are treated as one logical endpoint: a
// "POST /v1/chat/completions" entry also authorizes /v1/messages (an explicit
// "POST /v1/messages" entry authorizes only /v1/messages).
// read-only role restricts access to GET requests only.
// Returns false if the key is nil or the endpoint is not authorized.
func AuthorizeEndpoint(apiKey *config.ApiKey, method, path string) bool {
	if apiKey == nil {
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
		if Role(roleStr) == RoleReadOnly && method != "GET" {
			return false
		}
	}
	return true
}
