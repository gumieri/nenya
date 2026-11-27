package proxy

import (
	"net/http"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	mcpproxy "github.com/nenya/internal/proxy/mcp"
)

// NENYA-353 (Module A Phase 060): the tool-capability egress policy. A
// request whose content the interceptor chain marked sensitive — the
// redaction interceptor matched a secret pattern (SecretsRedacted) or the
// entropy interceptor redacted high-entropy spans (EntropyRedacted) — that
// also declares NETWORKED tools (declarations that can move data off-box:
// web search/fetch, HTTP request tools, mail/chat relays) is a live
// exfiltration channel: the model can copy sensitive values into a
// networked tool's arguments and the gateway's response-side guards
// (ExfilGuard, canary) never see that traffic, because it never flows back
// through the gateway.
//
// The policy runs on the chat dispatch path (both wire routes —
// /v1/chat/completions and /v1/messages share chainChat) AFTER the content
// pipeline (the sensitivity signal exists only then) and BEFORE dispatch,
// covering client-declared and gateway-injected (MCP) tools alike:
//
//   - log (default): warn + counter; the dispatch is unchanged.
//   - strip: the networked declarations are removed from payload["tools"]
//     and a tool_choice naming a removed tool is dropped with them; the
//     caller prunes the names from the injected-MCP bookkeeping too, so
//     the loop's execution gate refuses a model-initiated call to a
//     stripped tool (its system-prompt advertisement predates the policy
//     — the refusal, not the absence of the declaration, closes the
//     capability).
//   - block: the request is rejected 403 error_kind=exfil_blocked.
//
// The default is deliberately inert (log): stripping capabilities changes
// model behavior, so "strip"/"block" are operator opt-ins via
// governance.tool_egress. Classification is by tool NAME against globs
// (mcp.MatchesToolPattern semantics, the same matcher as the per-server
// tool_include/tool_exclude policy). Recorded boundary: /v1/responses is
// NOT covered — its pipeline (NENYA-346) does not run the interceptor
// chain, so it has no sensitivity signal to key the policy on; if the
// chain is ever extended there, this policy should follow.

// applyToolEgressPolicy enforces the tool-capability egress policy on the
// payload's tool declarations for a sensitive request. ok=false means the
// response was already written (the block action). stripped lists the
// networked names actually removed (nil when none) — the caller must
// prune them from any bookkeeping that gates EXECUTION, not just the
// declaration list: on the MCP path the gateway's loop executes calls
// whose names sit in the injected set (NENYA-162), so a declaration-only
// strip would leave the capability live.
func (p *Proxy) applyToolEgressPolicy(gw *gateway.NenyaGateway, w http.ResponseWriter, payload map[string]any, agentName string, sensitive bool) (stripped []string, ok bool) {
	if !sensitive || !gw.Config.Governance.EffectiveToolEgressEnabled() {
		return nil, true
	}
	networked := networkedToolNames(payload, gw.Config.Governance.EffectiveToolEgressPatterns())
	if len(networked) == 0 {
		return nil, true
	}
	action := gw.Config.Governance.EffectiveToolEgressAction()
	gw.Metrics.RecordToolEgressEvent(action)
	switch action {
	case "block":
		gw.Logger.Warn("tool egress policy blocked a sensitive request carrying networked tools",
			"agent", agentName, "tools", networked, "action", action)
		writeStructuredError(w, http.StatusForbidden, infra.ErrorKindExfil,
			"request declares networked tools while carrying sensitive values")
		return nil, false
	case "strip":
		strippedNames := stripNetworkedTools(payload, networked)
		gw.Metrics.RecordToolEgressStripped(len(strippedNames))
		gw.Logger.Warn("tool egress policy stripped networked tools from a sensitive request",
			"agent", agentName, "tools", strippedNames)
		return strippedNames, true
	default:
		gw.Logger.Warn("sensitive request declares networked tools",
			"agent", agentName, "tools", networked, "action", action)
		return nil, true
	}
}

// applyToolEgressOutcome runs the NENYA-353 tool-capability egress policy
// over the request from the pipeline outcome's sensitivity signals, and
// prunes a strip's names from the injected-MCP execution bookkeeping (the
// loop's gate keys on those names — NENYA-162; a declaration-only strip
// would leave the capability live). It reports false when the response
// was already written (the block action).
func (p *Proxy) applyToolEgressOutcome(gw *gateway.NenyaGateway, w http.ResponseWriter, req *chatRequest, outcome contentPipelineOutcome) bool {
	stripped, ok := p.applyToolEgressPolicy(gw, w, req.Payload, req.AgentName, outcome.SecretsRedacted || outcome.EntropyRedacted)
	if !ok {
		return false
	}
	if len(stripped) > 0 && len(req.InjectedMCPTools) > 0 {
		req.InjectedMCPTools = pruneInjectedMCPTools(req.InjectedMCPTools, stripped)
	}
	return true
}

// networkedToolNames returns the names of the payload's tool declarations
// matching any of the networked globs, in declaration order. Tool names
// are read from the two shapes the gateway dispatches — the OpenAI
// function wrapper (tools[].function.name) and flat name declarations
// (tools[].name, the Anthropic/Responses function shape). Non-map or
// nameless entries have no classifiable name and stay.
func networkedToolNames(payload map[string]any, patterns []string) []string {
	rawTools, _ := payload["tools"].([]interface{})
	if len(rawTools) == 0 || len(patterns) == 0 {
		return nil
	}
	var names []string
	seen := make(map[string]struct{}, len(rawTools))
	for _, raw := range rawTools {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := declaredToolName(entry)
		if name == "" {
			continue
		}
		if !toolNameNetworked(name, patterns) {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

// declaredToolName reads a declaration's name across the dispatched
// shapes: the OpenAI wrapper's function.name first, then the flat name.
func declaredToolName(entry map[string]any) string {
	if fn, ok := entry["function"].(map[string]any); ok {
		if name, ok := fn["name"].(string); ok && name != "" {
			return name
		}
	}
	name, _ := entry["name"].(string)
	return name
}

// toolNameNetworked reports whether a declaration name matches any of the
// networked globs.
func toolNameNetworked(name string, patterns []string) bool {
	for _, pat := range patterns {
		if mcpproxy.MatchesToolPattern(name, pat) {
			return true
		}
	}
	return false
}

// stripNetworkedTools removes the named networked declarations from
// payload["tools"] in place and drops a tool_choice that names one of
// them (an upstream would reject a forced choice naming an absent tool).
// It returns the names actually removed from the declaration list (the
// deduped networked set — every entry whose name is in it is gone).
func stripNetworkedTools(payload map[string]any, networked []string) []string {
	stripped := networkedNameSet(networked)
	rawTools, _ := payload["tools"].([]interface{})
	kept := make([]interface{}, 0, len(rawTools))
	for _, raw := range rawTools {
		entry, ok := raw.(map[string]any)
		if ok && isStrippedName(declaredToolName(entry), stripped) {
			continue
		}
		kept = append(kept, raw)
	}
	if len(kept) == 0 {
		delete(payload, "tools")
		// Any tool_choice is meaningless — and a provider 400 — without
		// declarations (string choices like the "auto" MCP injection
		// sets cannot name a survivor; there are none).
		delete(payload, "tool_choice")
	} else {
		payload["tools"] = kept
	}
	dropStrippedToolChoice(payload, stripped)
	return networked
}

// networkedNameSet builds the exact-name set for strip decisions.
func networkedNameSet(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

// isStrippedName reports whether a declaration name is in the stripped
// set (empty names never match).
func isStrippedName(name string, set map[string]struct{}) bool {
	if name == "" {
		return false
	}
	_, ok := set[name]
	return ok
}

// pruneInjectedMCPTools removes the stripped networked names from the
// gateway's injected-tool bookkeeping (order preserved): the MCP loop's
// execution gate answers any call to a pruned name with the structured
// `[unavailable tool]` refusal instead of dispatching it (NENYA-162).
// Recorded residual: the injected names were already advertised in the
// gateway-authored MCP system prompt (assembled before the policy ran);
// a model echoing one of them gets the refusal, not execution.
func pruneInjectedMCPTools(injected []string, stripped []string) []string {
	set := networkedNameSet(stripped)
	kept := make([]string, 0, len(injected))
	for _, name := range injected {
		if isStrippedName(name, set) {
			continue
		}
		kept = append(kept, name)
	}
	return kept
}

// dropStrippedToolChoice removes payload["tool_choice"] when it names a
// stripped tool (OpenAI {"type":"function","function":{"name":X}}, flat
// {"name":X}, and Anthropic-style {"type":"tool","name":X} shapes);
// string choices ("auto"/"none"/"required") always stay. It reports
// whether the field was dropped.
func dropStrippedToolChoice(payload map[string]any, stripped map[string]struct{}) bool {
	choice, ok := payload["tool_choice"].(map[string]any)
	if !ok {
		return false
	}
	if !isStrippedName(declaredToolName(choice), stripped) {
		return false
	}
	delete(payload, "tool_choice")
	return true
}
