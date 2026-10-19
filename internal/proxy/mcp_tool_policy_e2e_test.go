package proxy

import (
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/mcp"
	"github.com/nenya/internal/testutil"
)

// The NENYA-162 trust anchor end-to-end: the injected set the loop guards on
// comes from the gateway's own injection (registry routes + policy), never from
// the client-writable payload — so a tool cut by the server's exclude policy is
// refused by the real wiring, and the payload cannot re-declare it as injected.
func TestMCPToolPolicyTrustAnchorEndToEnd(t *testing.T) {
	mock := newTestMCPServer(t)
	// The server exposes BOTH tools: the exclude policy is what cuts one, so
	// deleting the policy must break this test.
	mock.mu.Lock()
	mock.tools = append(mock.tools, mcpToolDef{Name: "secret_tool", Description: "excluded by policy"})
	mock.handles["secret_tool"] = func(map[string]any) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "must never run"}}}
	}
	mock.mu.Unlock()

	client := mcp.NewClient(mcp.ClientConfig{
		Name:   "memory",
		URL:    mock.server.URL + "/sse",
		Logger: newTestLogger(),
	})
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.RefreshTools(t.Context()); err != nil {
		t.Fatalf("RefreshTools failed: %v", err)
	}

	index := mcp.NewToolRegistry()
	index.Register("memory", []mcp.Tool{
		{Name: "test_tool", Description: "A test tool"},
		{Name: "secret_tool", Description: "excluded by policy"},
	})

	gw := &gateway.NenyaGateway{
		Logger:       testutil.NewTestLogger(),
		Metrics:      &infra.Metrics{},
		MCPClients:   map[string]*mcp.Client{"memory": client},
		MCPToolIndex: index,
		Config: config.Config{
			MCPServers: map[string]config.MCPServerConfig{
				"memory": {ToolExclude: []string{"secret_tool"}},
			},
			Agents: map[string]config.AgentConfig{
				"agent": {MCP: &config.AgentMCPConfig{Servers: []string{"memory"}}},
			},
		},
	}

	p := &Proxy{}
	req := &chatRequest{
		AgentName: "agent",
		Agent:     gw.Config.Agents["agent"],
		// The client pre-declares the EXCLUDED tool: a payload-derived anchor
		// would treat it as injected, so this test fails if the anchor ever
		// regresses to the payload (NENYA-162).
		Payload: map[string]any{
			"messages": []any{},
			"tools": []any{
				map[string]any{"type": "function", "function": map[string]any{"name": "memory__secret_tool"}},
			},
		},
		apiKey: fullAccessTestKey(),
	}
	if !p.injectMCPTools(gw, req.Payload, req) {
		t.Fatal("expected the MCP tools to be injected")
	}

	if len(req.injectedMCPTools) != 1 || req.injectedMCPTools[0] != "memory__test_tool" {
		t.Fatalf("the injected set must hold exactly the policy-allowed tool: %v", req.injectedMCPTools)
	}

	// A call naming the excluded tool goes through the real wiring and is
	// refused with the structured NENYA-162 error (never dispatched).
	cut := "memory__secret_tool"
	unavailable := unavailableMCPToolCalls(
		[]mcpToolCall{{ID: "1", Name: cut}},
		injectedToolNameSet(req.injectedMCPTools),
	)
	if !unavailable[cut] {
		t.Fatalf("the excluded tool must be unavailable: %v", unavailable)
	}
	results := executeMCPCallsWithUnavailable(mcpExecOptions{
		ctx: t.Context(), calls: []mcpToolCall{{ID: "1", Name: cut}},
		gw: gw, agentName: "agent", apiKey: fullAccessTestKey(),
		unavailable: unavailable,
	})
	if len(results) != 1 || results[0] == nil || !results[0].IsError {
		t.Fatalf("the excluded tool must be refused: %+v", results)
	}
	if got := results[0].Text(); !strings.Contains(got, "[unavailable tool: "+cut+"]") {
		t.Fatalf("the refusal must name the tool: %q", got)
	}
}
