package auth

import "nenya/config"

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

// AuthorizeEndpoint checks if the key is allowed to access the given HTTP endpoint.
// Admin keys bypass endpoint restrictions.
// Custom allowed_endpoints override default role-based permissions.
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
		requested := method + " " + path
		for _, allowed := range apiKey.AllowedEndpoints {
			if allowed == requested {
				return true
			}
		}
		return false
	}

	for _, roleStr := range apiKey.Roles {
		if Role(roleStr) == RoleReadOnly && method != "GET" {
			return false
		}
	}
	return true
}
