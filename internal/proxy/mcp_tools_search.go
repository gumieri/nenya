package proxy

import (
	"math"
	"sort"
	"strings"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/mcp"
	"github.com/nenya/internal/pipeline"
)

// ---- Per-server tool policy (NENYA-162) -----------------------------------

// serverToolPolicy applies one server's include/exclude patterns to its granted
// tools: include (when non-empty) keeps only matching names, then exclude
// removes matching ones — exclude always wins. A pattern matches exactly or as
// a '*'/'?' glob. The policy runs AFTER the key grant, so the RBAC scope and
// the server policy compose.
func serverToolPolicy(tools []mcp.Tool, include, exclude []string) []mcp.Tool {
	if len(include) == 0 && len(exclude) == 0 {
		return tools
	}
	kept := make([]mcp.Tool, 0, len(tools))
	for _, tool := range tools {
		if len(include) > 0 && !matchesAnyPattern(tool.Name, include) {
			continue
		}
		if matchesAnyPattern(tool.Name, exclude) {
			continue // exclude wins over include
		}
		kept = append(kept, tool)
	}
	return kept
}

// matchesAnyPattern reports whether name matches any of the patterns.
func matchesAnyPattern(name string, patterns []string) bool {
	for _, pat := range patterns {
		if matchesToolPattern(name, pat) {
			return true
		}
	}
	return false
}

func matchesToolPattern(name, pattern string) bool {
	if !strings.ContainsAny(pattern, "*?") {
		return name == pattern
	}
	return globMatch(name, pattern)
}

// globMatch implements '*'/'?' over the tool name with a classic backtracking
// walk; no metacharacter has any other meaning.
func globMatch(name, pattern string) bool {
	ni, pi := 0, 0
	starName, starPat := -1, -1
	for ni < len(name) {
		switch {
		case pi < len(pattern) && (pattern[pi] == name[ni] || pattern[pi] == '?'):
			ni++
			pi++
		case pi < len(pattern) && pattern[pi] == '*':
			starName, starPat = ni, pi
			pi++
		case starPat >= 0:
			starName++
			ni = starName
			pi = starPat + 1
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// ---- Opt-in relevance-based tool selection (NENYA-162) ---------------------

// selectToolsForRequest implements the opt-in relevance-based MCP tool
// selection (NENYA-162): the granted tools are scored against the
// conversation's own terms and only the top `topN` (plus the agent's floor)
// are injected, instead of the whole catalog — token bloat on large servers,
// gone. Deterministic: same terms → same set (score desc, name-asc tiebreak),
// so the upstream tool surface stays stable per conversation. topN <= 0 passes
// the full granted set through (selection off); a no-signal conversation
// (no extractable terms) ALSO gets the full set ("selected_full_catalog") —
// without a query to be relevant against, selection cannot discriminate, and
// an arbitrary sub-delection would strand the agent. The outcome is returned
// with the set ("selected" otherwise). topN is clamped to the catalog size:
// an operator typo (a huge tool_search value) must not drive the capacity.
func selectToolsForRequest(tools []mcp.Tool, query string, topN int, floor []string) ([]mcp.Tool, string) {
	if topN <= 0 || len(tools) == 0 {
		return tools, ""
	}
	if topN > len(tools) {
		topN = len(tools)
	}
	terms := searchTerms(query)
	if len(terms) == 0 {
		return tools, "selected_full_catalog"
	}
	ordered := rankToolsForSelection(terms, tools)
	selected := headSelection(ordered, tools, topN)
	taken := make(map[string]struct{}, len(selected))
	for _, tool := range selected {
		taken[tool.Name] = struct{}{}
	}
	return applyToolFloor(selected, tools, floor, taken), "selected"
}

// rankToolsForSelection returns the tool indices by relevance: score desc,
// name-asc tiebreak. Scoring weights a name hit at 1.5 and a description hit
// at 1.0, scaled by inverse document frequency so rare matches dominate.
func rankToolsForSelection(terms []string, tools []mcp.Tool) []int {
	// Lower each tool's text ONCE: the df and scoring passes would otherwise
	// re-allocate it for every (term, tool) pair on the request path.
	names := make([]string, len(tools))
	descs := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = strings.ToLower(tool.Name)
		descs[i] = strings.ToLower(tool.Description)
	}
	df := make(map[string]int, len(terms))
	for _, term := range terms {
		for i := range tools {
			if strings.Contains(names[i], term) || strings.Contains(descs[i], term) {
				df[term]++
			}
		}
	}
	n := float64(len(tools))
	type scored struct {
		index int
		value float64
	}
	ranked := make([]scored, 0, len(tools))
	for i := range tools {
		value := 0.0
		for _, term := range terms {
			weight := 0.0
			if strings.Contains(names[i], term) {
				weight += 1.5 // the name is the highest-signal match
			}
			if strings.Contains(descs[i], term) {
				weight += 1.0
			}
			if weight > 0 {
				weight *= math.Log(1 + n/float64(df[term]))
			}
			value += weight
		}
		ranked = append(ranked, scored{index: i, value: value})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].value != ranked[j].value {
			return ranked[i].value > ranked[j].value
		}
		return tools[ranked[i].index].Name < tools[ranked[j].index].Name
	})
	ordered := make([]int, 0, len(ranked))
	for _, item := range ranked {
		ordered = append(ordered, item.index)
	}
	return ordered
}

// headSelection takes the topN tools in relevance order. When the whole
// catalog scores zero the head is still injected in name order (never empty,
// never index order), so the agent always has a surface and the set is stable
// for identical catalogs.
func headSelection(ordered []int, tools []mcp.Tool, topN int) []mcp.Tool {
	selected := make([]mcp.Tool, 0, topN)
	for _, idx := range ordered {
		if len(selected) >= topN {
			break
		}
		selected = append(selected, tools[idx])
	}
	return selected
}

// applyToolFloor appends the agent's always-include tools that selection left
// out. A floor entry already present (selected or appended) is skipped, so a
// duplicated floor list cannot inject the same function name twice.
func applyToolFloor(selected, tools []mcp.Tool, floor []string, taken map[string]struct{}) []mcp.Tool {
	for _, want := range floor {
		if _, ok := taken[want]; ok {
			continue
		}
		for _, tool := range tools {
			if tool.Name == want {
				selected = append(selected, tool)
				taken[want] = struct{}{}
				break
			}
		}
	}
	return selected
}

// searchTerms extracts the lowercase term list from the conversation's own
// text: distinct letters/digits/'_'/'-' runs, at most 32 terms of 24 runes —
// punctuation and code noise carry no signal and never reach the scoring.
func searchTerms(query string) []string {
	seen := make(map[string]struct{}, 32)
	terms := make([]string, 0, 32)
	for _, run := range strings.FieldsFunc(strings.ToLower(query), isNotWordRune) {
		if len(run) > 24 {
			run = run[:24]
		}
		if _, ok := seen[run]; ok {
			continue
		}
		seen[run] = struct{}{}
		terms = append(terms, run)
		if len(terms) >= 32 {
			break
		}
	}
	return terms
}

func isNotWordRune(r rune) bool {
	return r != '_' && r != '-' && (r < 'a' || r > 'z') && (r < '0' || r > '9')
}

// filterServerToolPolicy applies one server's include/exclude patterns from its
// mcp_servers config to the granted tools (NENYA-162). An unconfigured server
// has no policy: the grant passes through.
func filterServerToolPolicy(gw *gateway.NenyaGateway, serverName, agentName string, granted []mcp.Tool) []mcp.Tool {
	server, ok := gw.Config.MCPServers[serverName]
	if !ok || (len(server.ToolInclude) == 0 && len(server.ToolExclude) == 0) {
		return granted
	}
	before := len(granted)
	granted = serverToolPolicy(granted, server.ToolInclude, server.ToolExclude)
	gw.Logger.Info("MCP server tool policy applied",
		"server", serverName, "agent", agentName, "stage", "injection",
		"before", before, "after", len(granted))
	return granted
}

// selectConversationTools applies the agent's opt-in relevance-based selection
// (NENYA-162): the query is the conversation's own text (the most recent user
// turn, via the pipeline's prior-user-message extraction) and only the top
// `tool_search` (plus the agent's floor) are injected instead of the whole
// catalog. Same terms → same set, so the upstream tool surface is stable.
func selectConversationTools(gw *gateway.NenyaGateway, req *chatRequest, granted []mcp.Tool) []mcp.Tool {
	if req.Agent.MCP == nil || req.Agent.MCP.ToolSearch <= 0 {
		return granted
	}
	query := pipeline.ExtractPriorUserMessages(req.Messages, 1)
	selected, outcome := selectToolsForRequest(granted, query, req.Agent.MCP.ToolSearch, req.Agent.MCP.ToolSearchFloor)
	gw.Metrics.RecordMCPToolSearch(outcome)
	gw.Logger.Info("MCP tool selection",
		"agent", req.AgentName, "outcome", outcome,
		"catalog", len(granted), "injected", len(selected))
	return selected
}

// warnUnmatchedToolFloor reports a tool_search_floor entry that names no
// granted tool: the floor matches by exact name, so a typo would silently
// never apply (NENYA-162).
func warnUnmatchedToolFloor(gw *gateway.NenyaGateway, req *chatRequest, present map[string]struct{}) {
	if req.Agent.MCP == nil || len(req.Agent.MCP.ToolSearchFloor) == 0 {
		return
	}
	if len(present) == 0 {
		// Nothing was granted at all (every server unavailable or cut): the
		// server-level warnings already explain it, and a per-floor-entry
		// warning here would only add noise.
		return
	}
	for _, want := range req.Agent.MCP.ToolSearchFloor {
		if _, ok := present[want]; !ok {
			gw.Logger.Warn("mcp.tool_search_floor names no granted tool",
				"agent", req.AgentName, "tool", want)
		}
	}
}
