package proxy

import (
	"strconv"
	"strings"
	"testing"

	"github.com/nenya/config"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/mcp"
	"github.com/nenya/internal/testutil"
)

func TestMatchesToolPattern(t *testing.T) {
	cases := []struct {
		name, pattern string
		want          bool
	}{
		{"mcp__github__create_issue", "mcp__github__create_issue", true},
		{"mcp__github__create_issue", "mcp__github__create_*", true},
		{"mcp__github__list_issues", "mcp__github__create_*", false},
		{"search_files", "search_?", false},
		{"search_files", "search_?????", true},
		{"anything", "*", true},
		{"other", "", false},
		// Multiple stars, mixed star/question, trailing star, and the
		// star-backtracking path (a '*' that must give characters back).
		{"abc", "a*c", true},
		{"abbbc", "a*c", true},
		{"abbbc", "a*d", false},
		{"axcxc", "a*c*c", true},
		{"abc", "a*c*c", false},
		{"abcd", "a?c*", true},
		{"ab", "a?c*", false},
		{"anything", "**", true},
		{"", "*", true},
		{"", "?", false},
		{"suffix", "suffix*", true},
		{"prefix_mid", "prefix_*", true},
	}
	for _, tc := range cases {
		if got := matchesToolPattern(tc.name, tc.pattern); got != tc.want {
			t.Fatalf("matchesToolPattern(%q, %q) = %v, want %v", tc.name, tc.pattern, got, tc.want)
		}
	}
}

// Exclude wins over include; an include-only policy keeps matches.
func TestServerToolPolicyIncludeExclude(t *testing.T) {
	tools := []mcp.Tool{
		{Name: "create_issue", Description: "create an issue"},
		{Name: "list_issues", Description: "list issues"},
		{Name: "delete_repo", Description: "dangerous"},
	}
	kept := serverToolPolicy(tools, []string{"*_issue"}, []string{"list_*"})
	if len(kept) != 1 || kept[0].Name != "create_issue" {
		t.Fatalf("include+exclude policy: %+v", kept)
	}
	if got := serverToolPolicy(tools, nil, []string{"*"}); len(got) != 0 {
		t.Fatalf("exclude '*' empties the surface: %+v", got)
	}
	if got := serverToolPolicy(tools, nil, nil); len(got) != 3 {
		t.Fatalf("no policy passes through: %+v", got)
	}
}

func TestSelectToolsForRequestStableSet(t *testing.T) {
	tools := make([]mcp.Tool, 0, 40)
	for i := 0; i < 40; i++ {
		tools = append(tools, mcp.Tool{Name: "tool_" + strings.Repeat("x", i), Description: "generic"})
	}
	tools[len(tools)-1] = mcp.Tool{Name: "run_search", Description: "search the codebase for code"}
	query := "please search the codebase"

	a, _ := selectToolsForRequest(tools, query, 5, nil)
	b, _ := selectToolsForRequest(tools, query, 5, nil)
	if len(a) == 0 || len(b) == 0 {
		t.Fatalf("selection must never be empty: %d %d", len(a), len(b))
	}
	if len(a) > 5 || len(b) > 5 {
		t.Fatalf("selection respects the top-N: %d %d", len(a), len(b))
	}
	names := make([]string, 0, len(b))
	for _, tl := range b {
		names = append(names, tl.Name)
	}
	if len(names) == 0 || names[0] != "run_search" {
		t.Fatalf("the relevant tool must rank first: %v", names)
	}
	if len(a) != len(b) {
		t.Fatalf("identical terms must select the same count: %d vs %d", len(a), len(b))
	}
	for i, tl := range a {
		if tl.Name != b[i].Name {
			t.Fatalf("same terms must select the same set")
		}
	}
}

// The floor is always injected, even when relevance would exclude it.
func TestSelectToolsForRequestFloorAlwaysInjected(t *testing.T) {
	tools := []mcp.Tool{
		{Name: "run_search", Description: "search the codebase"},
		{Name: "delete_everything", Description: "irrelevant"},
	}
	query := "search the codebase for bugs"
	selected, outcome := selectToolsForRequest(tools, query, 1, []string{"delete_everything"})
	names := make([]string, 0, len(selected))
	for _, tl := range selected {
		names = append(names, tl.Name)
	}
	if len(selected) != 2 || outcome != "selected" {
		t.Fatalf("floor + top-N: %v %s", names, outcome)
	}
}

// No terms → the FULL granted catalog (nothing to discriminate against), and
// topN 0 = off (also the full set).
func TestSelectToolsForRequestNoSignal(t *testing.T) {
	tools := []mcp.Tool{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	selected, outcome := selectToolsForRequest(tools, "", 2, []string{"c"})
	if outcome != "selected_full_catalog" || len(selected) != len(tools) {
		t.Fatalf("no-terms selection must pass the catalog through: %+v %s", selected, outcome)
	}
	if got, _ := selectToolsForRequest(tools, "", 0, nil); len(got) != 3 {
		t.Fatalf("tool_search off passes the catalog through: %+v", got)
	}
}

// An operator typo (an enormous tool_search) must not panic or allocate the
// declared capacity: topN is clamped to the catalog size.
func TestSelectToolsForRequestHugeTopN(t *testing.T) {
	tools := []mcp.Tool{{Name: "a", Description: "alpha"}, {Name: "b", Description: "beta"}}
	selected, _ := selectToolsForRequest(tools, "alpha", 1<<62, nil)
	if len(selected) != 2 {
		t.Fatalf("a huge topN must clamp to the catalog: %+v", selected)
	}
}

// The unavailable set is anchored on the GATEWAY's injection bookkeeping, not
// on the client-writable payload (NENYA-162): a client that re-declares an
// excluded tool name in `tools` cannot make it count as injected.
func TestInjectedToolNameSetIsTheTrustAnchor(t *testing.T) {
	// The gateway injected only this tool...
	injected := injectedToolNameSet([]string{"mcp__s__allowed"})
	// ...while the client's payload also declares an excluded name.
	calls := []mcpToolCall{{Name: "mcp__s__allowed"}, {Name: "mcp__s__client_declared"}}
	un := unavailableMCPToolCalls(calls, injected)
	if un["mcp__s__allowed"] {
		t.Fatalf("a gateway-injected tool must not be refused: %v", un)
	}
	if !un["mcp__s__client_declared"] {
		t.Fatalf("a client-declared name must not count as injected: %v", un)
	}
	if injectedToolNameSet(nil) != nil {
		t.Fatal("no injection bookkeeping must leave the guard inert")
	}
}

// The unavailable-call set: a call naming a tool outside the injected set is
// refused, and a payload with no injected tools disables the guard.
func TestUnavailableMCPToolCalls(t *testing.T) {
	injected := map[string]struct{}{"mcp__s__ok": {}}
	calls := []mcpToolCall{
		{Name: "mcp__s__ok"},
		{Name: "mcp__s__hallucinated"},
	}
	un := unavailableMCPToolCalls(calls, injected)
	if !un["mcp__s__hallucinated"] || un["mcp__s__ok"] {
		t.Fatalf("unavailable set: %v", un)
	}
	if u := unavailableMCPToolCalls(calls, nil); u != nil {
		t.Fatalf("an empty injected set disables the guard: %v", u)
	}
}

// Leaner surface: selection on a large catalog injects measurably fewer tool
// declarations than the full set (NENYA-162 token-bloat acceptance).
func TestToolSelectionDropsInjectedTokens(t *testing.T) {
	t.Helper()
	tools := make([]mcp.Tool, 0, 200)
	for i := 0; i < 200; i++ {
		tools = append(tools, mcp.Tool{Name: "mcp__big__op_" + strconv.Itoa(i), Description: "does something"})
	}
	tools[7] = mcp.Tool{Name: "mcp__big__search_docs", Description: "search the docs for error handling"}
	gwl, _ := selectToolsForRequest(tools, "search docs about error handling", 8, nil)
	if got := len(gwl); got >= 20 {
		t.Fatalf("selection must be lean: got %d tools", got)
	}
	found := false
	for _, tl := range gwl {
		if tl.Name == "mcp__big__search_docs" {
			found = true
		}
	}
	if !found {
		t.Fatal("the ranked set must contain the relevant tool")
	}
}

// The loop refusal is REAL (NENYA-162 loop correctness): a call naming a tool
// outside the session's injected set produces a structured IsError result and
// never touches the tool index — while a nil set keeps the legacy pass-through.
func TestExecuteMCPCallsWithUnavailableRefuses(t *testing.T) {
	gw := &gateway.NenyaGateway{
		Logger:       testutil.NewTestLogger(),
		Metrics:      &infra.Metrics{},
		MCPToolIndex: mcp.NewToolRegistry(),
		Config: config.Config{Agents: map[string]config.AgentConfig{
			"agent": {MCP: &config.AgentMCPConfig{Servers: []string{"s"}}},
		}},
	}

	// The tool IS registered (the registry is global): the NENYA-162 guard,
	// not the unknown-tool path, must be what refuses it.
	index := mcp.NewToolRegistry()
	index.Register("s", []mcp.Tool{{Name: "cut_by_policy"}})
	gw.MCPToolIndex = index
	cutName := index.AllRoutes()[0].OpenAIToolName

	calls := []mcpToolCall{{ID: "1", Name: cutName}}
	results := executeMCPCallsWithUnavailable(mcpExecOptions{
		ctx: t.Context(), calls: calls, gw: gw, agentName: "agent",
		apiKey:      fullAccessTestKey(),
		unavailable: map[string]bool{cutName: true},
	})
	if len(results) != 1 || results[0] == nil || !results[0].IsError {
		t.Fatalf("the refusal must be a structured error result: %+v", results)
	}
	if got := results[0].Text(); !strings.Contains(got, "[unavailable tool: "+cutName+"]") {
		t.Fatalf("the refusal must name the tool: %q", got)
	}

	// nil set = legacy pass-through: the call falls through to the
	// pre-existing dispatch checks, never the NENYA-162 refusal.
	got := executeMCPCallsWithUnavailable(mcpExecOptions{
		ctx: t.Context(), calls: calls, gw: gw, agentName: "agent", apiKey: fullAccessTestKey(),
	})
	if len(got) != 1 || got[0] == nil || !got[0].IsError {
		t.Fatalf("the legacy path must keep its pre-existing error: %+v", got)
	}
	if text := got[0].Text(); strings.Contains(text, "[unavailable tool") {
		t.Fatalf("a nil unavailable set must not take the NENYA-162 guard: %q", text)
	}
}
