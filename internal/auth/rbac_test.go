package auth

import (
	"testing"

	"nenya/config"
)

// TestNilKeySafety verifies that auth functions correctly reject nil keys.
// With the concrete *config.ApiKey type, nil pointer detection works correctly
// via direct == nil comparison — no interface indirection footgun.
func TestNilKeySafety(t *testing.T) {
	var key *config.ApiKey

	if AuthorizeAgent(key, "any-agent") {
		t.Error("AuthorizeAgent of nil key should return false")
	}
	if AuthorizeEndpoint(key, "GET", "/v1/models") {
		t.Error("AuthorizeEndpoint of nil key should return false")
	}
}

func TestAuthorizeAgent(t *testing.T) {
	tests := []struct {
		name     string
		key      *config.ApiKey
		agent    string
		expected bool
	}{
		{
			name:     "admin bypass",
			key:      &config.ApiKey{Name: "admin", Roles: []string{"admin"}},
			agent:    "anything",
			expected: true,
		},
		{
			name:     "empty allowed agents allows all",
			key:      &config.ApiKey{Name: "user", Roles: []string{"user"}, AllowedAgents: nil},
			agent:    "general",
			expected: true,
		},
		{
			name:     "allowed agent matches",
			key:      &config.ApiKey{Name: "ci-bot", Roles: []string{"user"}, AllowedAgents: []string{"code-review", "ci-assistant"}},
			agent:    "code-review",
			expected: true,
		},
		{
			name:     "disallowed agent blocked",
			key:      &config.ApiKey{Name: "ci-bot", Roles: []string{"user"}, AllowedAgents: []string{"code-review", "ci-assistant"}},
			agent:    "general",
			expected: false,
		},
		{
			name:     "nil key returns false",
			key:      nil,
			agent:    "anything",
			expected: false,
		},
		{
			name:     "multiple roles with one admin",
			key:      &config.ApiKey{Name: "multi", Roles: []string{"user", "admin"}},
			agent:    "anything",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := AuthorizeAgent(tt.key, tt.agent)
			if result != tt.expected {
				t.Errorf("AuthorizeAgent(%v, %q) = %v; want %v", tt.key, tt.agent, result, tt.expected)
			}
		})
	}
}

func TestAuthorizeEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		key      *config.ApiKey
		method   string
		path     string
		expected bool
	}{
		{
			name:     "admin bypass",
			key:      &config.ApiKey{Name: "admin", Roles: []string{"admin"}},
			method:   "POST",
			path:     "/v1/chat/completions",
			expected: true,
		},
		{
			name:     "read-only GET models allowed",
			key:      &config.ApiKey{Name: "monitoring", Roles: []string{"read-only"}},
			method:   "GET",
			path:     "/v1/models",
			expected: true,
		},
		{
			name:     "read-only POST blocked",
			key:      &config.ApiKey{Name: "monitoring", Roles: []string{"read-only"}},
			method:   "POST",
			path:     "/v1/chat/completions",
			expected: false,
		},
		{
			name:     "user GET allowed",
			key:      &config.ApiKey{Name: "user", Roles: []string{"user"}},
			method:   "GET",
			path:     "/v1/models",
			expected: true,
		},
		{
			name:     "user POST allowed",
			key:      &config.ApiKey{Name: "user", Roles: []string{"user"}},
			method:   "POST",
			path:     "/v1/chat/completions",
			expected: true,
		},
		{
			name:     "custom allowed_endpoints matches",
			key:      &config.ApiKey{Name: "restricted", Roles: []string{"user"}, AllowedEndpoints: []string{"GET /v1/models", "POST /v1/chat/completions"}},
			method:   "GET",
			path:     "/v1/models",
			expected: true,
		},
		{
			name:     "custom allowed_endpoints blocks",
			key:      &config.ApiKey{Name: "restricted", Roles: []string{"user"}, AllowedEndpoints: []string{"GET /v1/models"}},
			method:   "POST",
			path:     "/v1/chat/completions",
			expected: false,
		},
		{
			name:     "nil key returns false",
			key:      nil,
			method:   "GET",
			path:     "/v1/models",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := AuthorizeEndpoint(tt.key, tt.method, tt.path)
			if result != tt.expected {
				t.Errorf("AuthorizeEndpoint(%v, %q, %q) = %v; want %v", tt.key, tt.method, tt.path, result, tt.expected)
			}
		})
	}
}

func TestHasPermission(t *testing.T) {
	tests := []struct {
		name     string
		role     Role
		perm     Permission
		expected bool
	}{
		{"admin has chat", RoleAdmin, PermissionChat, true},
		{"admin has admin", RoleAdmin, PermissionAdmin, true},
		{"user has chat", RoleUser, PermissionChat, true},
		{"user has models", RoleUser, PermissionModels, true},
		{"user does not have admin", RoleUser, PermissionAdmin, false},
		{"read-only has models", RoleReadOnly, PermissionModels, true},
		{"read-only does not have chat", RoleReadOnly, PermissionChat, false},
		{"unknown role has no permissions", Role("unknown"), PermissionChat, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := HasPermission(tt.role, tt.perm)
			if result != tt.expected {
				t.Errorf("HasPermission(%q, %q) = %v; want %v", tt.role, tt.perm, result, tt.expected)
			}
		})
	}
}

func TestAuthError(t *testing.T) {
	err := &AuthError{Status: 403, Msg: "forbidden"}
	expected := "auth: status=403 msg=forbidden"
	if err.Error() != expected {
		t.Errorf("AuthError.Error() = %q; want %q", err.Error(), expected)
	}

	if ErrKeyDisabled.Status != 403 {
		t.Errorf("ErrKeyDisabled.Status = %d; want 403", ErrKeyDisabled.Status)
	}
	if ErrKeyExpired.Status != 403 {
		t.Errorf("ErrKeyExpired.Status = %d; want 403", ErrKeyExpired.Status)
	}
}
