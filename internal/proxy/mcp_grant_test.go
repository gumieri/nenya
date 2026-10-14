package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/mcp"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/testutil"
)

// newMCPGrantProxy builds a gateway with one initialized MCP server exposing
// the helper's "test_tool", so grant capping can be exercised end to end
// through injectMCPTools/executeMCPCalls.
func newMCPGrantProxy(t *testing.T) *Proxy {
	t.Helper()
	mock := newTestMCPServer(t)
	client := mcp.NewClient(mcp.ClientConfig{
		Name:   "nenya-test",
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.RefreshTools(t.Context()); err != nil {
		t.Fatalf("RefreshTools failed: %v", err)
	}

	toolIndex := mcp.NewToolRegistry()
	toolIndex.Register("memory", client.ListTools())

	p := &Proxy{}
	p.StoreGateway(&gateway.NenyaGateway{
		Logger:       newTestLogger(),
		Metrics:      infra.NewMetrics(),
		MCPClients:   map[string]*mcp.Client{"memory": client},
		MCPToolIndex: toolIndex,
		Config: config.Config{
			Governance: config.GovernanceConfig{MCPGuard: &config.MCPGuardConfig{}},
			MCPServers: map[string]config.MCPServerConfig{
				"memory": {URL: mock.server.URL + "/sse"},
			},
			// NENYA-183: dispatch is bounded by the agent's own server list.
			Agents: map[string]config.AgentConfig{
				"agent": {MCP: &config.AgentMCPConfig{Servers: []string{"memory"}}},
			},
		},
	})
	return p
}

func grantedToolNames(payload map[string]any) []string {
	raw, _ := payload["tools"].([]interface{})
	names := make([]string, 0, len(raw))
	for _, item := range raw {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		if name, ok := fn["name"].(string); ok {
			names = append(names, name)
		}
	}
	return names
}

func mcpAgentRequest(p *Proxy, key *config.ApiKey) *chatRequest {
	agent := config.AgentConfig{
		Strategy: "fallback",
		MCP:      &config.AgentMCPConfig{Servers: []string{"memory"}},
	}
	gw := p.Gateway()
	gw.Config.Agents = map[string]config.AgentConfig{"agent": agent}
	return &chatRequest{
		Payload:   map[string]any{},
		ModelName: "agent",
		AgentName: "agent",
		Agent:     agent,
		apiKey:    key,
	}
}

// TestInjectMCPToolsCappedByKeyGrant pins the NENYA-183 injection cap: a key
// only ever sees the tools inside the intersection of its grant and the
// agent's configured servers.
func TestInjectMCPToolsCappedByKeyGrant(t *testing.T) {
	p := newMCPGrantProxy(t)
	gw := p.Gateway()

	tests := []struct {
		name   string
		grant  []string
		expect []string
	}{
		{"empty grant allows every exposed tool", nil, []string{"memory__test_tool"}},
		{"exact namespaced entry", []string{"memory__test_tool"}, []string{"memory__test_tool"}},
		{"server wildcard", []string{"memory__*"}, []string{"memory__test_tool"}},
		{"bare tool name", []string{"test_tool"}, []string{"memory__test_tool"}},
		{"unrelated entry caps everything out", []string{"other__tool"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key := &config.ApiKey{Name: "k", Roles: []string{"user"}, Enabled: true, AllowedTools: tc.grant}
			req := mcpAgentRequest(p, key)
			p.injectMCPTools(gw, req.Payload, req)

			got := grantedToolNames(req.Payload)
			if len(got) != len(tc.expect) {
				t.Fatalf("injected tools = %v, want %v", got, tc.expect)
			}
			for i := range got {
				if got[i] != tc.expect[i] {
					t.Fatalf("injected tools = %v, want %v", got, tc.expect)
				}
			}
			if len(tc.expect) == 0 {
				if _, has := req.Payload["tool_choice"]; has {
					t.Error("no tools injected but tool_choice was set")
				}
			}
		})
	}
}

// TestExecuteMCPCallsEnforcesKeyGrant pins dispatch-time enforcement: a model
// naming a tool outside the key's grant is refused with a tool error even
// though the tool exists in the registry.
func TestExecuteMCPCallsEnforcesKeyGrant(t *testing.T) {
	p := newMCPGrantProxy(t)
	gw := p.Gateway()
	call := mcpToolCall{ID: "call_1", Name: "memory__test_tool", Arguments: map[string]any{"query": "hi"}}

	restricted := &config.ApiKey{Name: "restricted", Roles: []string{"user"}, Enabled: true, AllowedTools: []string{"other__tool"}}
	results := executeMCPCalls(t.Context(), []mcpToolCall{call}, gw, "agent", pipeline.CanarResult{}, nil, restricted)
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("expected a refused tool result, got %+v", results)
	}
	if !strings.Contains(results[0].Text(), "not permitted") {
		t.Errorf("refusal text = %q, want it to name the grant", results[0].Text())
	}

	granted := &config.ApiKey{Name: "granted", Roles: []string{"user"}, Enabled: true, AllowedTools: []string{"memory__test_tool"}}
	results = executeMCPCalls(t.Context(), []mcpToolCall{call}, gw, "agent", pipeline.CanarResult{}, nil, granted)
	if len(results) != 1 || results[0].IsError {
		t.Fatalf("granted tool call must succeed, got %+v", results)
	}
}

// TestCheckMCPAvailabilityFailsClosed pins the credential fail-closed rule: an
// agent selecting a server recorded as unusable for a credential reason fails
// the request with a structured error instead of silently dropping its tools.
func TestCheckMCPAvailabilityFailsClosed(t *testing.T) {
	p := newMCPGrantProxy(t)
	gw := p.Gateway()
	req := mcpAgentRequest(p, fullAccessTestKey())

	if herr := p.checkMCPAvailability(gw, req); herr != nil {
		t.Fatalf("healthy server must not fail the request: %v", herr)
	}

	gw.MarkMCPUnavailable("memory", "missing required header Authorization")
	herr := p.checkMCPAvailability(gw, req)
	if herr == nil {
		t.Fatal("expected a fail-closed error for a credential-less server")
	}
	if herr.Code != http.StatusServiceUnavailable || herr.Kind != infra.ErrorKindMCPUnavailable {
		t.Fatalf("got %d/%s, want 503/%s", herr.Code, herr.Kind, infra.ErrorKindMCPUnavailable)
	}
	if strings.Contains(herr.Message, "memory") {
		t.Errorf("the client-facing message must not enumerate configured servers, got %q", herr.Message)
	}
	if reason, ok := gw.MCPUnavailableReason("memory"); !ok || !strings.Contains(reason, "Authorization") {
		t.Errorf("the reason must stay available to operators, got %q (ok=%v)", reason, ok)
	}
}

// TestGuardMCPArgsEnforcesKeyGrant pins that the non-registry dispatches
// (auto-search, auto-save) are capped by the same grant.
func TestGuardMCPArgsEnforcesKeyGrant(t *testing.T) {
	p := newMCPGrantProxy(t)
	gw := p.Gateway()

	restricted := &config.ApiKey{Name: "restricted", Roles: []string{"user"}, Enabled: true, AllowedTools: []string{"other__*"}}
	if err := guardMCPArgs(gw, gw.Logger, mcpGuardDispatch{
		ServerName: "memory", ToolName: "test_tool", Purpose: "auto_search",
		Args: map[string]any{"query": "hi"}, ApiKey: restricted,
	}); !errors.Is(err, errMCPNotGranted) {
		t.Fatalf("an ungranted auto-search dispatch must be refused with the grant sentinel, got %v", err)
	}

	granted := &config.ApiKey{Name: "granted", Roles: []string{"user"}, Enabled: true, AllowedTools: []string{"memory__*"}}
	if err := guardMCPArgs(gw, gw.Logger, mcpGuardDispatch{
		ServerName: "memory", ToolName: "test_tool", Purpose: "auto_search",
		Args: map[string]any{"query": "hi"}, ApiKey: granted,
	}); err != nil {
		t.Fatalf("a granted auto-search dispatch must be allowed, got %v", err)
	}

	// A nil key cannot be scoped: fail closed.
	if err := guardMCPArgs(gw, gw.Logger, mcpGuardDispatch{
		ServerName: "memory", ToolName: "test_tool", Purpose: "auto_save",
		Args: map[string]any{"content": "hi"},
	}); !errors.Is(err, errMCPNotGranted) {
		t.Fatalf("a dispatch without an authenticated key must be refused, got %v", err)
	}

	// An argument-policy rejection is distinguishable from a grant refusal.
	if err := guardMCPArgs(gw, gw.Logger, mcpGuardDispatch{
		ServerName: "memory", ToolName: "test_tool", Purpose: "auto_search",
		Args: map[string]any{"query": "x", "url": "http://169.254.169.254/latest/meta-data/"}, ApiKey: granted,
	}); !errors.Is(err, errMCPArgumentPolicy) {
		t.Fatalf("a private-URL dispatch must be refused by the argument policy, got %v", err)
	}
}

// TestExecuteMCPCallsRefusesServerOutsideAgentConfig pins the outer bound of
// the grant: the tool registry is global, so a model naming another server's
// tool must be refused even though the tool exists and the key has no cap.
func TestExecuteMCPCallsRefusesServerOutsideAgentConfig(t *testing.T) {
	p := newMCPGrantProxy(t)
	gw := p.Gateway()

	otherIndex := mcp.NewToolRegistry()
	otherIndex.Register("other", []mcp.Tool{{Name: "other_tool"}})
	gw.MCPToolIndex = otherIndex

	results := executeMCPCalls(t.Context(), []mcpToolCall{{
		ID: "call_1", Name: "other__other_tool", Arguments: map[string]any{},
	}}, gw, "agent", pipeline.CanarResult{}, nil, fullAccessTestKey())
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("expected a refusal for a server outside the agent's config, got %+v", results)
	}
	if !strings.Contains(results[0].Text(), "not configured for this agent") {
		t.Errorf("refusal text = %q, want it to name the agent boundary", results[0].Text())
	}

	// An unknown agent is refused too (fail closed).
	results = executeMCPCalls(t.Context(), []mcpToolCall{{
		ID: "call_2", Name: "other__other_tool", Arguments: map[string]any{},
	}}, gw, "ghost-agent", pipeline.CanarResult{}, nil, fullAccessTestKey())
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("expected a refusal for an unresolvable agent, got %+v", results)
	}
}

// TestAutoSaveRefusedOutsideKeyGrant pins that the operator-configured
// auto-save dispatch is capped by the caller's grant too.
func TestAutoSaveRefusedOutsideKeyGrant(t *testing.T) {
	p := newMCPGrantProxy(t)
	gw := p.Gateway()
	agent := config.AgentConfig{MCP: &config.AgentMCPConfig{
		Servers:  []string{"memory"},
		AutoSave: true,
		SaveTool: "test_tool",
	}}

	restricted := &config.ApiKey{Name: "restricted", Roles: []string{"user"}, Enabled: true, AllowedTools: []string{"other__*"}}
	if p.autoSaveTryServer(gw, &agent, "memory", "agent", "a conversation summary", restricted) {
		t.Fatal("auto-save must be refused when the save tool is outside the key's grant")
	}

	granted := &config.ApiKey{Name: "granted", Roles: []string{"user"}, Enabled: true, AllowedTools: []string{"memory__test_tool"}}
	if !p.autoSaveTryServer(gw, &agent, "memory", "agent", "a conversation summary", granted) {
		t.Fatal("auto-save must succeed for a granted save tool")
	}
}

// TestMCPUnavailableFailsTheRequestClosed drives the whole request path: an
// agent whose MCP server is unusable for a credential reason must be answered
// with a structured 503 (and the same answer whether or not a cache entry
// exists, since the check runs before the cache lookup).
func TestMCPUnavailableFailsTheRequestClosed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		writeCleanChatStream(w)
	}))
	defer up.Close()

	p := newSessionProxy(t, up.URL)
	gw := p.Gateway()
	agent := gw.Config.Agents["test-agent"]
	agent.MCP = &config.AgentMCPConfig{Servers: []string{"memory"}}
	gw.Config.Agents["test-agent"] = agent
	gw.MarkMCPUnavailable("memory", "missing required header Authorization")

	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`)
	req.Header.Set("Authorization", "Bearer key-a-token-1234567890")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusServiceUnavailable)
	body := rec.Body.String()
	if !strings.Contains(body, string(infra.ErrorKindMCPUnavailable)) {
		t.Fatalf("expected error_kind=mcp_unavailable, got %s", body)
	}
	if strings.Contains(body, "Authorization") {
		t.Errorf("the client response must not name the missing header: %s", body)
	}
}

// TestGuardMCPArgsRefusesUnusableServer pins that the non-registry dispatches
// share the credential gate: a server marked unusable cannot be reached by
// auto-search or auto-save either.
func TestGuardMCPArgsRefusesUnusableServer(t *testing.T) {
	p := newMCPGrantProxy(t)
	gw := p.Gateway()
	gw.MarkMCPCredentialRejected("memory", "credential rejected")

	err := guardMCPArgs(gw, gw.Logger, mcpGuardDispatch{
		ServerName: "memory", ToolName: "test_tool", Purpose: "auto_search",
		Args: map[string]any{"query": "hi"}, ApiKey: fullAccessTestKey(),
	})
	if !errors.Is(err, errMCPServerUnavailable) {
		t.Fatalf("expected the unavailability sentinel, got %v", err)
	}

	// A permanent misconfiguration is refused the same way.
	gw.ClearMCPUnavailable("memory")
	gw.MarkMCPUnavailable("memory", "missing required header Authorization")
	err = guardMCPArgs(gw, gw.Logger, mcpGuardDispatch{
		ServerName: "memory", ToolName: "test_tool", Purpose: "auto_save",
		Args: map[string]any{"content": "hi"}, ApiKey: fullAccessTestKey(),
	})
	if !errors.Is(err, errMCPServerUnavailable) {
		t.Fatalf("expected the unavailability sentinel, got %v", err)
	}
}

// TestCheckMCPAvailabilitySkipsServerOutsideKeyGrant pins that a key whose
// grant cannot reach the affected server is not failed by that server's
// health: it could not call its tools even if it were healthy.
func TestCheckMCPAvailabilitySkipsServerOutsideKeyGrant(t *testing.T) {
	p := newMCPGrantProxy(t)
	gw := p.Gateway()
	gw.MarkMCPUnavailable("memory", "missing required header Authorization")

	unaffected := &config.ApiKey{Name: "other", Roles: []string{"user"}, Enabled: true, AllowedTools: []string{"files__*"}}
	req := mcpAgentRequest(p, unaffected)
	if herr := p.checkMCPAvailability(gw, req); herr != nil {
		t.Fatalf("a key whose grant excludes the server must not be failed by it: %v", herr)
	}

	affected := &config.ApiKey{Name: "memory-key", Roles: []string{"user"}, Enabled: true, AllowedTools: []string{"memory__*"}}
	req = mcpAgentRequest(p, affected)
	if herr := p.checkMCPAvailability(gw, req); herr == nil {
		t.Fatal("a key whose grant reaches the unusable server must be failed closed")
	}

	// A bare entry could match the server's tools, so it is treated as
	// affected (conservative).
	bare := &config.ApiKey{Name: "bare", Roles: []string{"user"}, Enabled: true, AllowedTools: []string{"test_tool"}}
	req = mcpAgentRequest(p, bare)
	if herr := p.checkMCPAvailability(gw, req); herr == nil {
		t.Fatal("a bare grant entry must be treated as reaching the server")
	}
}
