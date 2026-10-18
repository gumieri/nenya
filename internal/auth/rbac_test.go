package auth

import (
	"net/http"
	"testing"

	"github.com/nenya/config"
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
			name:     "read-only GET telemetry allowed",
			key:      &config.ApiKey{Name: "monitoring", Roles: []string{"read-only"}},
			method:   "GET",
			path:     "/statsz",
			expected: true,
		},
		{
			name:     "read-only GET proxy passthrough blocked (NENYA-131 path scoping)",
			key:      &config.ApiKey{Name: "monitoring", Roles: []string{"read-only"}},
			method:   "GET",
			path:     "/proxy/openai/v1/models",
			expected: false,
		},
		{
			name:     "read-only GET pprof blocked (NENYA-131 path scoping)",
			key:      &config.ApiKey{Name: "monitoring", Roles: []string{"read-only"}},
			method:   "GET",
			path:     "/debug/pprof/",
			expected: false,
		},
		{
			name:     "read-only GET files blocked (NENYA-131 path scoping)",
			key:      &config.ApiKey{Name: "monitoring", Roles: []string{"read-only"}},
			method:   "GET",
			path:     "/v1/files",
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
			name:     "chat entry grants messages route (NENYA-52)",
			key:      &config.ApiKey{Name: "restricted", Roles: []string{"user"}, AllowedEndpoints: []string{"POST /v1/chat/completions"}},
			method:   "POST",
			path:     "/v1/messages",
			expected: true,
		},
		{
			name:     "explicit messages entry grants messages route",
			key:      &config.ApiKey{Name: "restricted", Roles: []string{"user"}, AllowedEndpoints: []string{"POST /v1/messages"}},
			method:   "POST",
			path:     "/v1/messages",
			expected: true,
		},
		{
			name:     "explicit messages entry does not grant chat route",
			key:      &config.ApiKey{Name: "restricted", Roles: []string{"user"}, AllowedEndpoints: []string{"POST /v1/messages"}},
			method:   "POST",
			path:     "/v1/chat/completions",
			expected: false,
		},
		{
			name:     "method-mismatched chat entry does not grant messages route",
			key:      &config.ApiKey{Name: "restricted", Roles: []string{"user"}, AllowedEndpoints: []string{"GET /v1/chat/completions"}},
			method:   "POST",
			path:     "/v1/messages",
			expected: false,
		},
		{
			name:     "key holding both chat entries passes messages route",
			key:      &config.ApiKey{Name: "restricted", Roles: []string{"user"}, AllowedEndpoints: []string{"POST /v1/chat/completions", "POST /v1/messages"}},
			method:   "POST",
			path:     "/v1/messages",
			expected: true,
		},
		{
			name:     "key holding both chat entries passes chat route",
			key:      &config.ApiKey{Name: "restricted", Roles: []string{"user"}, AllowedEndpoints: []string{"POST /v1/chat/completions", "POST /v1/messages"}},
			method:   "POST",
			path:     "/v1/chat/completions",
			expected: true,
		},
		{
			name:     "non-POST entry does not grant messages route",
			key:      &config.ApiKey{Name: "restricted", Roles: []string{"user"}, AllowedEndpoints: []string{"GET /v1/models"}},
			method:   "POST",
			path:     "/v1/messages",
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

// TestAuthorizeEndpointPathOnlyIdentity pins the endpoint-classification
// contract behind NENYA-161 (GoModel #1038): an endpoint's identity is
// method + PATH only, and the caller must supply the query-free path
// (production passes r.URL.Path, which excludes the query). The query-free
// rows are the invariant; the query-bearing rows document the caller contract —
// the matcher is a literal method+path comparison, so a query-bearing identity
// would silently break every allowlist entry. If the matcher ever gains its own
// query normalization, these caller-contract rows should flip to true (the
// end-to-end assertions in internal/proxy/handler_test.go would then still
// hold). The test also pins the adjacent path-shape rules the invariant
// depends on (exact matching, no trailing-slash normalization, no
// percent-decoding inside the matcher) and the query interaction with the
// canonical chat-route mapping (NENYA-52) and the NENYA-131 read-only scoping.
func TestAuthorizeEndpointPathOnlyIdentity(t *testing.T) {
	restricted := &config.ApiKey{Name: "restricted", Roles: []string{"user"}, AllowedEndpoints: []string{"GET /v1/models"}}
	writer := &config.ApiKey{Name: "writer", Roles: []string{"user"}, AllowedEndpoints: []string{"POST /v1/chat/completions"}}
	monitoring := &config.ApiKey{Name: "monitoring", Roles: []string{"read-only"}}

	tests := []struct {
		name     string
		key      *config.ApiKey
		method   string
		path     string
		expected bool
	}{
		{
			name:     "allowlist entry matches its query-free path",
			key:      restricted,
			method:   "GET",
			path:     "/v1/models",
			expected: true,
		},
		{
			// Caller contract: production passes r.URL.Path, so this shape never
			// reaches the matcher there.
			name:     "caller contract: a query-bearing path is not an endpoint identity",
			key:      restricted,
			method:   "GET",
			path:     "/v1/models?foo=1",
			expected: false,
		},
		{
			name:     "caller contract: a multi-parameter query is not an endpoint identity",
			key:      restricted,
			method:   "GET",
			path:     "/v1/models?a=b&c=d",
			expected: false,
		},
		{
			name:     "caller contract: a query cannot smuggle a non-allowlisted path",
			key:      restricted,
			method:   "GET",
			path:     "/v1/chat/completions?foo=1",
			expected: false,
		},
		{
			// Trailing slashes are distinct paths: matching is exact and
			// therefore fail-closed (a slash-suffixed request is denied).
			name:     "a trailing slash does not match an allowlist entry",
			key:      restricted,
			method:   "GET",
			path:     "/v1/models/",
			expected: false,
		},
		{
			// The matcher does no decoding of its own; the caller supplies the
			// decoded r.URL.Path (which is what makes /v1/%6dodels authorize).
			name:     "the matcher does not decode percent-encoding",
			key:      restricted,
			method:   "GET",
			path:     "/v1/%6dodels",
			expected: false,
		},
		{
			// Same caller-contract shape for %2F: the raw encoded string is not
			// an allowlist identity (in production %2F decodes into a path
			// separator, so the decoded form is a different path; that decoding
			// is pinned end to end in internal/proxy/handler_test.go).
			name:     "caller contract: a raw encoded slash is not an allowlist identity",
			key:      restricted,
			method:   "GET",
			path:     "/v1/models%2Ffoo",
			expected: false,
		},
		{
			// The canonical chat-route mapping (NENYA-52) also switches on the
			// exact path, so it is query-sensitive in the same way.
			name:     "canonical chat entry matches its query-free path",
			key:      writer,
			method:   "POST",
			path:     "/v1/chat/completions",
			expected: true,
		},
		{
			name:     "canonical chat entry grants the query-free messages route",
			key:      writer,
			method:   "POST",
			path:     "/v1/messages",
			expected: true,
		},
		{
			name:     "caller contract: the canonical chat mapping is query-sensitive",
			key:      writer,
			method:   "POST",
			path:     "/v1/chat/completions?foo=1",
			expected: false,
		},
		{
			name:     "caller contract: the messages mapping is query-sensitive",
			key:      writer,
			method:   "POST",
			path:     "/v1/messages?foo=1",
			expected: false,
		},
		{
			// NENYA-131 read-only scoping, re-confirmed for the query shape
			// (the query-free cases live in TestAuthorizeEndpoint).
			name:     "read-only query-bearing path denied (NENYA-131 path scoping)",
			key:      monitoring,
			method:   "GET",
			path:     "/v1/models?foo=1",
			expected: false,
		},
		{
			name:     "read-only trailing slash denied (NENYA-131 path scoping)",
			key:      monitoring,
			method:   "GET",
			path:     "/v1/models/",
			expected: false,
		},
		{
			name:     "read-only proxy passthrough with a query denied (NENYA-131 path scoping)",
			key:      monitoring,
			method:   "GET",
			path:     "/proxy/openai/v1/models?foo=1",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AuthorizeEndpoint(tt.key, tt.method, tt.path); got != tt.expected {
				t.Errorf("AuthorizeEndpoint(%q, %q, %q) = %v; want %v", tt.key.Name, tt.method, tt.path, got, tt.expected)
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

// TestAuthorizeUnknownRoleDenies pins the NENYA-196 fail-closed role check:
// a key carrying an unrecognized role (or no role at all) must not fall
// through to the permissive branches. Load validation rejects such keys, so
// this is the runtime defense-in-depth layer.
func TestAuthorizeUnknownRoleDenies(t *testing.T) {
	unknown := &config.ApiKey{Name: "unknown-role", Roles: []string{"superuser"}, Enabled: true}
	if AuthorizeAgent(unknown, "any-agent") {
		t.Error("unknown role must not authorize an agent")
	}
	if AuthorizeEndpoint(unknown, http.MethodGet, "/v1/models") {
		t.Error("unknown role must not authorize an endpoint")
	}
	if AuthorizeEndpoint(unknown, http.MethodPost, "/v1/chat/completions") {
		t.Error("unknown role must not authorize the chat endpoint")
	}

	roleless := &config.ApiKey{Name: "roleless", Enabled: true}
	if AuthorizeAgent(roleless, "any-agent") || AuthorizeEndpoint(roleless, http.MethodGet, "/v1/models") {
		t.Error("a key with no roles must be denied")
	}

	mixed := &config.ApiKey{Name: "mixed", Roles: []string{"user", "bogus"}, Enabled: true}
	if AuthorizeAgent(mixed, "any-agent") {
		t.Error("a key mixing a known and an unknown role must be denied")
	}

	known := &config.ApiKey{Name: "known", Roles: []string{"user"}, Enabled: true}
	if !AuthorizeAgent(known, "any-agent") {
		t.Error("a recognized role must still authorize")
	}
	if !AuthorizeEndpoint(known, http.MethodPost, "/v1/chat/completions") {
		t.Error("a recognized role must still authorize the chat endpoint")
	}
}

// TestAuthorizeMCPTool pins the NENYA-183 per-key tool grant: the grant caps
// what the agent's MCP configuration exposes, and an ungranted or unscopable
// caller is denied.
func TestAuthorizeMCPTool(t *testing.T) {
	base := func(tools []string) *config.ApiKey {
		return &config.ApiKey{Name: "k", Roles: []string{"user"}, Enabled: true, AllowedTools: tools}
	}

	tests := []struct {
		name  string
		key   *config.ApiKey
		allow bool
	}{
		{"nil key denies", nil, false},
		{"empty grant allows everything", base(nil), true},
		{"exact namespaced entry", base([]string{"memory__search"}), true},
		{"server wildcard", base([]string{"memory__*"}), true},
		{"bare tool name", base([]string{"search"}), true},
		{"different server denies", base([]string{"files__search"}), false},
		{"different tool on same server denies", base([]string{"memory__save"}), false},
		{"wildcard of another server denies", base([]string{"files__*"}), false},
		{"unrecognized role denies", &config.ApiKey{Name: "bad", Roles: []string{"superuser"}, Enabled: true}, false},
		{"roleless key denies", &config.ApiKey{Name: "none", Enabled: true}, false},
		{"admin bypasses the cap", &config.ApiKey{Name: "root", Roles: []string{"admin"}, Enabled: true, AllowedTools: []string{"other__x"}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AuthorizeMCPTool(tc.key, "memory", "search"); got != tc.allow {
				t.Fatalf("AuthorizeMCPTool = %v, want %v", got, tc.allow)
			}
		})
	}
}

// TestMCPGrantTouchesServer pins the conservative reachability test used to
// decide whether a server's credential health matters to a key (NENYA-183).
func TestMCPGrantTouchesServer(t *testing.T) {
	tests := []struct {
		name   string
		key    *config.ApiKey
		server string
		want   bool
	}{
		{"nil key", nil, "memory", false},
		{"empty grant reaches every server", &config.ApiKey{Name: "k", Roles: []string{"user"}, Enabled: true}, "memory", true},
		{"namespaced entry reaches its server", &config.ApiKey{Name: "k", Roles: []string{"user"}, Enabled: true, AllowedTools: []string{"memory__search"}}, "memory", true},
		{"wildcard reaches its server", &config.ApiKey{Name: "k", Roles: []string{"user"}, Enabled: true, AllowedTools: []string{"memory__*"}}, "memory", true},
		{"other server does not", &config.ApiKey{Name: "k", Roles: []string{"user"}, Enabled: true, AllowedTools: []string{"files__*"}}, "memory", false},
		{"bare entry could reach any server", &config.ApiKey{Name: "k", Roles: []string{"user"}, Enabled: true, AllowedTools: []string{"search"}}, "memory", true},
		{"admin reaches every server", &config.ApiKey{Name: "k", Roles: []string{"admin"}, Enabled: true, AllowedTools: []string{"files__*"}}, "memory", true},
		{"unrecognized role", &config.ApiKey{Name: "k", Roles: []string{"superuser"}, Enabled: true}, "memory", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MCPGrantTouchesServer(tc.key, tc.server); got != tc.want {
				t.Fatalf("MCPGrantTouchesServer = %v, want %v", got, tc.want)
			}
		})
	}
}

// NENYA-195: bring-your-own-key permission. Admin keys hold it implicitly;
// every other key must opt in explicitly, and a nil key never may.
func TestKeyMayBYOK(t *testing.T) {
	cases := []struct {
		name string
		key  *config.ApiKey
		want bool
	}{
		{"nil key", nil, false},
		{"admin", &config.ApiKey{Roles: []string{"admin"}}, true},
		{"user without the permission", &config.ApiKey{Roles: []string{"user"}}, false},
		{"read-only", &config.ApiKey{Roles: []string{"read-only"}}, false},
		{"user opted in", &config.ApiKey{Roles: []string{"user"}, Permissions: map[string]any{"byok": true}}, true},
		{"user opted out explicitly", &config.ApiKey{Roles: []string{"user"}, Permissions: map[string]any{"byok": false}}, false},
		{"non-boolean permission fails closed", &config.ApiKey{Roles: []string{"user"}, Permissions: map[string]any{"byok": "yes"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := KeyMayBYOK(tc.key); got != tc.want {
				t.Fatalf("KeyMayBYOK = %v, want %v", got, tc.want)
			}
		})
	}
}

// The permission is not granted by any non-admin role by default.
func TestPermissionBYOKNotGrantedByDefaultRoles(t *testing.T) {
	for _, role := range []Role{RoleUser, RoleReadOnly} {
		if HasPermission(role, PermissionBYOK) {
			t.Fatalf("role %s must not hold PermissionBYOK by default", role)
		}
	}
	if !HasPermission(RoleAdmin, PermissionBYOK) {
		t.Fatal("admin must hold PermissionBYOK")
	}
}
