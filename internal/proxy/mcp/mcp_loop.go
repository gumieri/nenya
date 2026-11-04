package mcpproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/auth"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/mcp"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
	"github.com/nenya/internal/tracing"
	"github.com/nenya/internal/util"
)

// injectMCPTools injects the agent's MCP tools into the upstream request,
// capped by the caller's per-key tool grant (NENYA-183): a key only ever sees
// (and therefore can only invoke) tools inside the intersection of its
// allowed_tools and the agent's configured servers. Servers unusable for a
// credential reason are rejected earlier, in checkMCPAvailability. It reads
// AgentName (logging identity) and Agent (resolved config) from the same
// chatRequest, so they cannot diverge, and reports whether any MCP tool was
// injected — which selects the buffered tool loop.
func InjectMCPTools(h Host, gw *gateway.NenyaGateway, payload map[string]interface{}, req *chatRequest) bool {
	agent := req.Agent
	if agent.MCP == nil || len(agent.MCP.Servers) == 0 {
		return false
	}
	agentName := req.AgentName

	gw.Logger.Info("MCP injection starting",
		"servers", agent.MCP.Servers, "agent", agentName)

	var toolNames []string
	grantedNames := make(map[string]struct{})
	existing, _ := payload["tools"].([]interface{})
	for _, serverName := range agent.MCP.Servers {
		openaiTools, granted := serverInjectedTools(h, gw, serverName, req)
		if len(openaiTools) == 0 {
			continue
		}
		for _, tool := range granted {
			grantedNames[tool.Name] = struct{}{}
		}
		for _, t := range openaiTools {
			existing = append(existing, t)
		}
		// The advertised names come from the REGISTRY ROUTES (not from
		// re-parsing the declarations): the trust anchor must not depend on
		// the declaration's wire shape.
		toolNames = append(toolNames, advertisedToolNames(gw, serverName, granted)...)
		payload["tools"] = existing
		gw.Logger.Debug("MCP tools injected",
			"server", serverName, "tools", len(granted), "agent", agentName)
	}

	// The floor is validated against the union of every server's granted tools:
	// a per-server check would warn about a floor tool that a DIFFERENT server
	// of the same agent grants (NENYA-162).
	warnUnmatchedToolFloor(gw, req, grantedNames)
	req.InjectedMCPTools = toolNames

	if len(toolNames) > 0 {
		if _, has := payload["tool_choice"]; !has {
			payload["tool_choice"] = "auto"
			gw.Logger.Info("MCP tool_choice auto injected",
				"tools_count", len(toolNames), "agent", agentName)
		}
		InjectMCPSystemPrompt(h, gw, payload, toolNames, h.SpotlightSettingsFor(&gw.Config, agent))
		return true
	}
	gw.Logger.Warn("MCP: no tools injected for agent",
		"agent", agentName, "servers", agent.MCP.Servers)
	return false
}

// serverInjectedTools resolves one server's contribution to the injected set:
// server availability, the caller's key grant, the server's include/exclude
// policy, and (opt-in) relevance-based selection — in that order, so RBAC
// scope and policy compose (NENYA-162). It returns nil when the server
// contributes nothing, logging why.
func serverInjectedTools(h Host, gw *gateway.NenyaGateway, serverName string, req *chatRequest) ([]map[string]any, []mcp.Tool) {
	agentName := req.AgentName
	client, ok := gw.MCPClients[serverName]
	if !ok || !client.Ready() {
		gw.Logger.Warn("MCP server not available, skipping tool injection",
			"server", serverName, "agent", agentName)
		return nil, nil
	}
	tools := client.ListTools()
	if len(tools) == 0 {
		gw.Logger.Warn("MCP server returned no tools",
			"server", serverName, "agent", agentName)
		return nil, nil
	}
	granted := grantedToolsForServer(gw, req.APIKey, serverName, agentName, tools)
	if len(granted) == 0 {
		gw.Logger.Warn("MCP server exposes no tools inside the key's grant",
			"server", serverName, "agent", agentName)
		return nil, nil
	}
	granted = filterServerToolPolicy(gw, serverName, agentName, granted)
	if len(granted) == 0 {
		gw.Logger.Warn("MCP server exposes no tools after the server's include/exclude policy",
			"server", serverName, "agent", agentName)
		return nil, nil
	}
	granted = selectConversationTools(gw, req, granted)
	// Build from the registry so the advertised function name and the
	// dispatch key are the same string (NENYA-172).
	openaiTools := gw.MCPToolIndex.OpenAITools(serverName, granted)
	h.ApplyToolDescriptionSpotlight(openaiTools, h.SpotlightSettingsFor(&gw.Config, req.Agent), gw.Metrics)
	return openaiTools, granted
}

// advertisedToolNames maps the granted tools of one server to the advertised
// function names the registry uses for them (NENYA-172: the advertised name and
// the dispatch key are the same string). The registry is the single source: the
// same accessor backs the declarations, so a granted tool can never be
// advertised under a name the guard would then refuse.
func advertisedToolNames(gw *gateway.NenyaGateway, serverName string, granted []mcp.Tool) []string {
	names := make([]string, 0, len(granted))
	for _, tool := range granted {
		names = append(names, gw.MCPToolIndex.AdvertisedName(serverName, tool.Name))
	}
	return names
}

// grantedToolsForServer returns the server's tools that the key's grant covers
// (NENYA-183). An empty grant returns every tool, so the filter is a no-op for
// keys that do not cap their tools.
func grantedToolsForServer(gw *gateway.NenyaGateway, key *config.ApiKey, serverName, agentName string, tools []mcp.Tool) []mcp.Tool {
	granted := make([]mcp.Tool, 0, len(tools))
	for _, tool := range tools {
		if !auth.AuthorizeMCPTool(key, serverName, tool.Name) {
			gw.Logger.Debug("MCP tool outside the key's grant, not injected",
				"server", serverName, "tool", tool.Name, "agent", agentName)
			continue
		}
		granted = append(granted, tool)
	}
	return granted
}

// checkMCPAvailability fails the request closed (NENYA-183) when the agent
// selects an MCP server that cannot be used for a credential reason: a missing
// declared required header, or a server that refused the configured credential
// (401/403). Silently dropping such a server's tools would turn a
// misconfiguration into an unauthenticated or tool-less request.
func CheckMCPAvailability(h Host, gw *gateway.NenyaGateway, req *chatRequest) *httpError {
	if req.Agent.MCP == nil {
		return nil
	}
	for _, serverName := range req.Agent.MCP.Servers {
		// A key whose grant cannot reach the server is unaffected by it: it
		// could not call its tools even if the server were healthy.
		if !auth.MCPGrantTouchesServer(req.APIKey, serverName) {
			continue
		}
		reason, unavailable := gw.MCPUnavailableReason(serverName)
		if !unavailable {
			continue
		}
		// The reason stays in the log and in /statsz: the client gets a
		// generic message so an authenticated caller cannot enumerate
		// configured servers or missing headers.
		gw.Logger.Error("MCP server unusable for a credential reason, failing request closed",
			"server", serverName, "agent", req.AgentName, "reason", reason)
		gw.Metrics.IncMCPUnavailable(serverName)
		return &httpError{
			Code:    http.StatusServiceUnavailable,
			Message: "MCP tool server unavailable",
			Kind:    infra.ErrorKindMCPUnavailable,
		}
	}
	return nil
}

func InjectMCPSystemPrompt(h Host, gw *gateway.NenyaGateway, payload map[string]interface{}, toolNames []string, settings pipeline.SpotlightSettings) {
	toolsList := util.JoinBackticks(toolNames)

	prompt := fmt.Sprintf(
		"You have access to the following MCP tools for long-term memory and knowledge retrieval: %s. "+
			"Use these tools when the user asks about previously discussed information, needs to recall past "+
			"conversations, or explicitly requests memory/knowledge operations. Do NOT mention these tools "+
			"unless the user's query requires accessing stored information.",
		toolsList,
	)

	messages, ok := payload["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		return
	}

	preamble := ""
	if settings.Enabled {
		// The envelope rule must reach the model whenever envelopes do
		// (arXiv:2403.14720 efficacy depends on the rule being present).
		// Tradeoff: mixed setups (Nenya-managed MCP + client-side tool
		// history) carry the rule twice — the spotlight interceptor adds
		// its own copy ahead of the first history envelope. Accepted for
		// placement robustness (see CONFIGURATION.md).
		preamble = "\n\n" + pipeline.SpotlightPreamble
	}
	mcpMsg := map[string]interface{}{
		"role":    "system",
		"content": prompt + preamble,
	}

	updated := make([]interface{}, 0, util.AddCap(len(messages), 1))
	updated = append(updated, mcpMsg)
	updated = append(updated, messages...)
	payload["messages"] = updated

	gw.Logger.Debug("MCP system prompt injected", "tools", len(toolNames))
}

func discoverToolByPrefix(h Host, gw *gateway.NenyaGateway, serverName, prefix string) string {
	client, ok := gw.MCPClients[serverName]
	if !ok {
		return ""
	}
	for _, tool := range client.ListTools() {
		if strings.Contains(tool.Name, prefix) {
			return tool.Name
		}
	}
	return ""
}

// injectAutoSearch performs a best-effort auto-search against the request's
// agent's MCP servers and injects the result as memory context. Reads
// AgentName (logging/metrics identity) and Agent (resolved config) from the
// same chatRequest, so they cannot diverge.
func InjectAutoSearch(h Host, gw *gateway.NenyaGateway, ctx context.Context, payload map[string]interface{}, messages []interface{}, req *chatRequest) {
	agent := req.Agent
	agentName := req.AgentName
	if agent.MCP == nil || !agent.MCP.AutoSearch {
		return
	}

	query, _ := extractAutoSearchQuery(h, messages)
	if query == "" {
		return
	}

	query = redactQuery(h, gw, query)

	for _, serverName := range agent.MCP.Servers {
		if !canPerformAutoSearch(h, gw, serverName) {
			continue
		}

		toolName := resolveSearchTool(h, gw, serverName, agent.MCP.SearchTool, agentName)
		if toolName == "" {
			continue
		}

		if result := executeAutoSearch(h, gw, ctx, serverName, toolName, query, agentName, req.APIKey); result != nil {
			InjectAutoSearchContext(h, gw, AutoSearchContextOpts{
				Payload:    payload,
				Messages:   messages,
				ServerName: serverName,
				Result:     result,
				ToolName:   toolName,
				AgentName:  agentName,
				Settings:   h.SpotlightSettingsFor(&gw.Config, agent),
			})
			break
		}
	}
}

func extractAutoSearchQuery(h Host, messages []interface{}) (string, map[string]interface{}) {
	if len(messages) == 0 {
		return "", nil
	}
	lastMsg, ok := messages[len(messages)-1].(map[string]interface{})
	if !ok {
		return "", nil
	}
	lastRole, _ := lastMsg["role"].(string)
	if lastRole != "user" {
		return "", nil
	}
	query := gateway.ExtractContentText(lastMsg)
	return query, lastMsg
}

func redactQuery(h Host, gw *gateway.NenyaGateway, query string) string {
	query = pipeline.RedactSecrets(query, (gw.Config.Bouncer.Enabled != nil && *gw.Config.Bouncer.Enabled), gw.SecretPatterns, gw.Config.Bouncer.RedactionLabel)
	if gw.EntropyFilter != nil {
		query = gw.EntropyFilter.RedactHighEntropy(query, gw.Config.Bouncer.RedactionLabel)
	}
	return query
}

func canPerformAutoSearch(h Host, gw *gateway.NenyaGateway, serverName string) bool {
	client, ok := gw.MCPClients[serverName]
	return ok && client.Ready()
}

func resolveSearchTool(h Host, gw *gateway.NenyaGateway, serverName, configuredTool, agentName string) string {
	if configuredTool != "" {
		return configuredTool
	}
	toolName := discoverToolByPrefix(h, gw, serverName, "search")
	if toolName == "" {
		gw.Logger.Warn("MCP auto-search: no 'search' tool found on server",
			"server", serverName, "agent", agentName)
	}
	return toolName
}

func executeAutoSearch(h Host, gw *gateway.NenyaGateway, ctx context.Context, serverName, toolName, query, agentName string, apiKey *config.ApiKey) *autoSearchResult {
	start := time.Now()
	result, err := mcpClientCallTool(h, gw, ctx, serverName, toolName, query, apiKey)
	duration := time.Since(start)

	if err != nil {
		if errors.Is(err, errMCPArgumentPolicy) {
			// The guard already logged the rejection at Warn.
			gw.Logger.Debug("MCP auto-search skipped by argument guard",
				"server", serverName, "agent", agentName)
		} else {
			gw.Logger.Warn("MCP auto-search failed, proceeding without",
				"server", serverName, "agent", agentName, "err", err,
				"duration_ms", duration.Milliseconds())
		}
		gw.Metrics.RecordMCPAutoSearch(serverName, agentName, false, err)
		return nil
	}
	if result == nil || result.Text() == "" {
		gw.Logger.Debug("MCP auto-search: no results",
			"server", serverName, "agent", agentName,
			"duration_ms", duration.Milliseconds())
		gw.Metrics.RecordMCPAutoSearch(serverName, agentName, false, nil)
		return nil
	}

	return &AutoSearchResult{
		Text:      redactSearchResult(h, gw, result.Text()),
		ToolName:  toolName,
		Duration:  duration,
		Server:    serverName,
		AgentName: agentName,
	}
}

func mcpClientCallTool(h Host, gw *gateway.NenyaGateway, ctx context.Context, serverName, toolName, query string, apiKey *config.ApiKey) (*mcp.CallToolResult, error) {
	client, ok := gw.MCPClients[serverName]
	if !ok {
		return nil, fmt.Errorf("MCP client not found")
	}
	args := map[string]any{
		"query": query,
		"limit": 5,
	}
	// The argument guard applies to auto-search too: the query carries
	// untrusted conversation-derived text. A rejection surfaces as an
	// error so executeAutoSearch records a failed search and injects
	// nothing into the conversation.
	if guardErr := guardMCPArgs(gw, gw.Logger, MCPGuardDispatch{
		APIKey:     apiKey,
		ServerName: serverName,
		ToolName:   toolName,
		Purpose:    "auto_search",
		Args:       args,
	}); guardErr != nil {
		return nil, guardErr
	}
	return client.CallTool(ctx, toolName, args)
}

func redactSearchResult(h Host, gw *gateway.NenyaGateway, resultText string) string {
	resultText = pipeline.RedactSecrets(resultText, (gw.Config.Bouncer.Enabled != nil && *gw.Config.Bouncer.Enabled), gw.SecretPatterns, gw.Config.Bouncer.RedactionLabel)
	if gw.EntropyFilter != nil {
		resultText = gw.EntropyFilter.RedactHighEntropy(resultText, gw.Config.Bouncer.RedactionLabel)
	}
	return resultText
}

// InjectAutoSearchContext appends the auto-search memory context block to
// the request's last user message (delimiters-only spotlighting per the
// MCP integration contract) and records the auto-search metric.
func InjectAutoSearchContext(h Host, gw *gateway.NenyaGateway, opts AutoSearchContextOpts) {
	contextStr := fmt.Sprintf("[Memory context from %s]\n%s", opts.ServerName, opts.Result.Text)
	contextStr = opts.Settings.ApplyDelimitersOnly(contextStr, pipeline.SpotlightSourceMemory(opts.ServerName))
	memoryMsg := map[string]interface{}{
		"role":    "system",
		"content": contextStr,
	}

	updated := make([]interface{}, 0, util.AddCap(1, len(opts.Messages)))
	updated = append(updated, opts.Messages[:len(opts.Messages)-1]...)
	updated = append(updated, memoryMsg)
	updated = append(updated, opts.Messages[len(opts.Messages)-1:]...)
	opts.Payload["messages"] = updated

	if opts.Settings.Enabled {
		gw.Metrics.RecordSpotlighted("memory:" + opts.ServerName)
	}
	gw.Logger.Debug("MCP auto-search context injected",
		"server", opts.ServerName, "agent", opts.AgentName,
		"tool", opts.ToolName,
		"duration_ms", opts.Result.Duration.Milliseconds(),
		"result_len", len(opts.Result.Text))
	gw.Metrics.RecordMCPAutoSearch(opts.ServerName, opts.AgentName, true, nil)
}

// ForwardToUpstreamWithMCP runs the MCP multi-turn loop for a request whose
// payload carries MCP tool declarations: injects the granted tools, loops
// dispatch → tool-call extraction → gateway-side execution → tool-result
// append (with the NENYA-162 injected-tools guard), and replays the final
// buffered response to the client.
func ForwardToUpstreamWithMCP(h Host, gw *gateway.NenyaGateway,
	w http.ResponseWriter,
	r *http.Request,
	opts ForwardOptions) {
	maxIter := mcpMaxIterations
	if opts.Agent.MCP != nil && opts.Agent.MCP.MaxIterations > 0 {
		maxIter = opts.Agent.MCP.MaxIterations
		if maxIter > mcpMaxIterationsHardCeiling {
			maxIter = mcpMaxIterationsHardCeiling
		}
	}

	originalPayload, err := json.Marshal(opts.Payload)
	if err != nil {
		gw.Logger.Error("failed to marshal payload for MCP loop", "err", err)
		h.WriteSSEError(gw, w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// NENYA-188: the MCP loop dispatches one upstream call per iteration and
	// bypasses the chat retry loop, so the caller's daily key budget is
	// reserved here — once per logical request, non-refundable (NENYA-20). The
	// provider daily budget is checked per dispatch by checkPreDispatchGuards,
	// which the loop's dispatches go through.
	if reason, denied := h.ReserveKeyBudget(gw, opts.APIKey, opts.TokenCount); denied {
		gw.Metrics.IncAuthDenials(h.APIKeyName(opts.APIKey), reason)
		gw.Logger.Warn("MCP loop rejected: token budget exhausted",
			"agent", opts.AgentName, "reason", reason, "token_count", opts.TokenCount)
		// The MCP loop always replays SSE (even for stream:false), so the
		// denial matches that shape with the same error_kind as the chat paths
		// (NENYA-188).
		h.WriteGatewayStreamError(w, http.StatusTooManyRequests, ErrorTypeRateLimit, "Token budget exhausted ("+reason+")")
		return
	}

	var lastBuf *bufferedSSE
	loopStart := time.Now()
	totalToolCalls := 0
	actualIter := 0

	// NENYA-140: the whole tool loop is one traced stage; thread the loop
	// span back into r so re-dispatches parent to it, not the root.
	// Gateway-rooted: the multi-turn loop is work the gateway initiates, so
	// it gets its own trace with the client's context linked (NENYA-187).
	loopCtx, loopSpan := tracing.StartGatewaySpan(r.Context(), gw.Logger, "upstream.mcp_loop")
	defer loopSpan.End()
	tracing.RecordStage(loopCtx, "upstream.mcp_loop")
	r = r.WithContext(loopCtx)
	mcpLoopCtx, mcpLoopCancel := context.WithTimeout(loopCtx, mcpLoopMaxDuration)
	defer mcpLoopCancel()

	defer func() {
		loopDuration := time.Since(loopStart)
		if loopDuration > 0 {
			gw.Metrics.RecordMCPLoopDuration(opts.AgentName, loopDuration)
		}
		gw.Logger.Info("MCP multi-turn loop completed",
			"agent", opts.AgentName,
			"iterations", actualIter,
			"tool_calls_executed", totalToolCalls,
			"duration_ms", loopDuration.Milliseconds())
	}()

loop:
	for iteration := range maxIter {
		in := mcpIterInput{
			H:               h,
			GW:              gw,
			MCPLoopCtx:      mcpLoopCtx,
			W:               w,
			R:               r,
			Opts:            opts,
			Iteration:       iteration,
			OriginalPayload: &originalPayload,
			LastBuf:         &lastBuf,
			ActualIter:      &actualIter,
			TotalToolCalls:  &totalToolCalls,
		}
		switch mcpIteration(h, in) {
		case mcpIterContinue:
		case mcpIterReturn:
			return
		case mcpIterStop:
			break loop
		}
	}

	if lastBuf != nil {
		gw.Logger.Warn("MCP loop exhausted, replaying last response",
			"max_iterations", maxIter, "agent", opts.AgentName)
		ReplayGuardedBufferedWithProvider(h, gw, w, lastBuf, lastBufProviderName(lastBuf), opts.AgentName, opts.SourceFormat, opts.APIKey)
		RecordMCPUsage(h, gw, lastBuf, opts.AgentName)
		return
	}

	h.WriteStructuredError(w, http.StatusInternalServerError, infra.ErrorKindInternal, "MCP loop ended without response")
}

const (
	mcpIterContinue = iota
	mcpIterReturn
	mcpIterStop
)

type MCPIterInput struct {
	// H is carried for signature uniformity with the per-iteration helpers
	// that take the host explicitly; the loop bodies thread the host
	// parameter rather than reading this field.
	H               Host
	GW              *gateway.NenyaGateway
	MCPLoopCtx      context.Context
	W               http.ResponseWriter
	R               *http.Request
	Opts            ForwardOptions
	Iteration       int
	OriginalPayload *[]byte
	LastBuf         **BufferedSSE
	ActualIter      *int
	TotalToolCalls  *int
}

// handleMCPBufferOutcome resolves a buffered upstream outcome that
// terminates the iteration without further processing: an exfil-blocked
// buffer (block payload written) or an upstream failure (last response
// replayed / gateway error written). Returns true when the caller must
// return immediately.
func handleMCPBufferOutcome(h Host, in mcpIterInput, buf *bufferedSSE, err error) bool {
	if err == nil && buf != nil && buf.ExfilBlocked {
		// The guard truncated this turn's buffer: terminate the whole
		// loop with the block payload — continuing would execute tool
		// calls or fold reasoning derived from violating content.
		in.GW.Logger.Warn("MCP loop: response blocked by exfil guard",
			"iteration", in.Iteration, "agent", in.Opts.AgentName)
		h.WriteExfilBlockedSSE(in.GW, in.W)
		return true
	}
	if err == nil && buf != nil && buf.CanaryBlocked {
		// Same termination for the canary tripwire, with the
		// exfil_detected error kind.
		in.GW.Logger.Warn("MCP loop: response blocked by canary tripwire",
			"iteration", in.Iteration, "agent", in.Opts.AgentName)
		h.WriteExfilDetectedSSE(in.GW, in.W)
		return true
	}
	return false
}

func mcpIteration(h Host, in mcpIterInput) int {
	select {
	case <-in.MCPLoopCtx.Done():
		in.GW.Logger.Warn("MCP loop deadline exceeded", "agent", in.Opts.AgentName, "iterations", *in.ActualIter)
		if *in.LastBuf != nil {
			// The replayed buffer is a dispatch that reached the provider; its
			// usage was claimed by recordMCPIterationUsage at its own
			// iteration, so this call is an idempotent no-op (NENYA-193).
			RecordMCPUsage(h, in.GW, *in.LastBuf, in.Opts.AgentName)
			ReplayGuardedBufferedWithProvider(h, in.GW, in.W, *in.LastBuf, lastBufProviderName(*in.LastBuf), in.Opts.AgentName, in.Opts.SourceFormat, in.Opts.APIKey)
		} else {
			h.WriteSSEError(in.GW, in.W, http.StatusRequestTimeout, "MCP loop deadline exceeded")
		}
		return mcpIterReturn
	default:
	}

	in.GW.Metrics.RecordMCPLoopIteration(in.Opts.AgentName)
	(*in.ActualIter)++

	working := make(map[string]interface{})
	if in.Iteration == 0 {
		// Iteration 0 re-dispatches the already-pipelined request payload
		// verbatim; unmarshaling the original bytes first would be wasted.
		working = in.Opts.Payload
	} else if err := json.Unmarshal(*in.OriginalPayload, &working); err != nil {
		in.GW.Logger.Error("failed to unmarshal payload for MCP iteration", "err", err)
		return mcpIterStop
	}

	buf, err := ForwardBuffered(h, in.GW, in.MCPLoopCtx, in.R, bufferedDispatch{
		Targets:         in.Opts.Targets,
		Payload:         working,
		Cooldown:        in.Opts.Cooldown,
		TokenCount:      in.Opts.TokenCount,
		AgentName:       in.Opts.AgentName,
		MaxRetries:      in.Opts.MaxRetries,
		APIKey:          in.Opts.APIKey,
		Canary:          in.Opts.Canary,
		SourceFormat:    in.Opts.SourceFormat,
		WindowCompacted: in.Opts.WindowCompacted,
	})
	// NENYA-135: each loop iteration dispatched one upstream request whose
	// cl100k estimate was recorded at dispatch — record this iteration's
	// real usage here (nil-buf safe; before the handled check, so
	// exfil/canary-blocked buffers with a valid usage payload still pair)
	// so K estimates pair with K actuals instead of biasing the ratio
	// toward ~1/K (the terminal paths only add Stats).
	recordCalibrationFromBuffer(in.GW, mcpCalibrationModel(in.Opts.Targets, buf), buf)
	// NENYA-193: this iteration is one upstream dispatch that reached a
	// provider, so it claims its usage (Stats, metrics, cost, one billing
	// request) exactly once. The terminal paths below are idempotent no-ops
	// for a buffer claimed here.
	RecordMCPIterationUsage(h, in.MCPLoopCtx, in.GW, buf, in.Opts.Targets, in.Opts.AgentName)
	if handled := handleMCPBufferOutcome(h, in, buf, err); handled {
		return mcpIterReturn
	}
	if err != nil {
		in.GW.Logger.Warn("MCP loop: upstream failed, streaming last response",
			"iteration", in.Iteration, "err", err)
		if *in.LastBuf != nil {
			ReplayGuardedBufferedWithProvider(h, in.GW, in.W, *in.LastBuf, lastBufProviderName(*in.LastBuf), in.Opts.AgentName, in.Opts.SourceFormat, in.Opts.APIKey)
			return mcpIterReturn
		}
		h.WriteSSEError(in.GW, in.W, http.StatusBadGateway, "All upstream providers failed")
		return mcpIterReturn
	}

	allCalls := buf.ToolCalls
	if len(allCalls) == 0 {
		in.GW.Logger.Debug("MCP loop: content-only response, replaying",
			"has_content", buf.HasContent,
			"finish_reason", buf.FinishReason,
			"raw_bytes_len", len(buf.RawBytes))
		ReplayGuardedBufferedWithProvider(h, in.GW, in.W, buf, lastBufProviderName(buf), in.Opts.AgentName, in.Opts.SourceFormat, in.Opts.APIKey)
		RecordMCPUsage(h, in.GW, buf, in.Opts.AgentName)
		return mcpIterReturn
	}

	mcpCalls, nonMcpCalls := partitionMCPToolCalls(allCalls, in.GW.MCPToolIndex)
	*in.TotalToolCalls += len(mcpCalls)

	if len(mcpCalls) > 0 {
		in.GW.Logger.Info("MCP tool calls intercepted",
			"mcp_calls", len(mcpCalls),
			"non_mcp_calls", len(nonMcpCalls),
			"iteration", in.Iteration+1,
			"agent", in.Opts.AgentName)

		if !executeAndAppendMCPResults(h, in, working, mcpCalls, buf) {
			return mcpIterReturn
		}

		updatedPayload, err := json.Marshal(working)
		if err != nil {
			in.GW.Logger.Error("failed to marshal updated payload for MCP loop", "err", err)
			RecordMCPUsage(h, in.GW, buf, in.Opts.AgentName)
			ReplayGuardedBufferedWithProvider(h, in.GW, in.W, buf, lastBufProviderName(buf), in.Opts.AgentName, in.Opts.SourceFormat, in.Opts.APIKey)
			return mcpIterReturn
		}
		*in.OriginalPayload = updatedPayload
	}

	if len(mcpCalls) == 0 && len(nonMcpCalls) > 0 {
		in.GW.Logger.Debug("MCP loop: non-MCP tool calls only, replaying",
			"non_mcp_calls", len(nonMcpCalls),
			"raw_bytes_len", len(buf.RawBytes))
		ReplayGuardedBufferedWithProvider(h, in.GW, in.W, buf, lastBufProviderName(buf), in.Opts.AgentName, in.Opts.SourceFormat, in.Opts.APIKey)
		RecordMCPUsage(h, in.GW, buf, in.Opts.AgentName)
		return mcpIterReturn
	}

	*in.LastBuf = buf
	return mcpIterContinue
}

// injectedToolNameSet converts the gateway's injected-name list into a set
// (NENYA-162). Nil/empty means no injection bookkeeping: the guard is inert.
func injectedToolNameSet(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}

// unavailableMCPToolCalls reports which calls name an MCP tool the session did
// not inject (NENYA-162): the loop refuses them with a structured error result.
// An empty injected set leaves nothing unavailable (defensive: the guard is
// anchored on the gateway's own injection bookkeeping, not the payload).
func unavailableMCPToolCalls(calls []mcpToolCall, injected map[string]struct{}) map[string]bool {
	if len(injected) == 0 {
		return nil
	}
	unavailable := make(map[string]bool, len(calls))
	for _, c := range calls {
		if _, ok := injected[c.Name]; !ok {
			unavailable[c.Name] = true
		}
	}
	return unavailable
}

// executeAndAppendMCPResults executes the intercepted MCP tool calls,
// appends their results (assistant tool_calls message + per-call tool
// messages) to the working conversation, and re-scans the appended content
// through the deterministic security interceptors (NENYA-136). Returns
// false when the loop must stop: the rescan was rejected or failed, and the
// structured client error (or deadline replay) has already been written.
// The iteration's buffered usage is recorded on that abort path so /statsz
// does not undercount.
func executeAndAppendMCPResults(h Host, in mcpIterInput, working map[string]any, mcpCalls []mcpToolCall, buf *bufferedSSE) bool {
	results := executeMCPCallsWithUnavailable(h, MCPExecOptions{
		Ctx:         in.MCPLoopCtx,
		Calls:       mcpCalls,
		GW:          in.GW,
		AgentName:   in.Opts.AgentName,
		Canary:      in.Opts.Canary,
		Screen:      in.Opts.Screen,
		APIKey:      in.Opts.APIKey,
		Unavailable: unavailableMCPToolCalls(mcpCalls, injectedToolNameSet(in.Opts.InjectedMCPTools)),
	})
	mcpAssistantMsg := map[string]any{
		"role":       "assistant",
		"content":    nil,
		"tool_calls": buildOpenAIToolCalls(mcpCalls),
	}
	if buf.ReasoningContent != "" {
		mcpAssistantMsg["reasoning_content"] = buf.ReasoningContent
	}
	appended := appendMCPResults(working, mcpCalls, results, mcpAssistantMsg, MCPAppendContext{
		Settings:  h.SpotlightSettingsFor(&in.GW.Config, in.Opts.Agent),
		Metrics:   in.GW.Metrics,
		ToolIndex: in.GW.MCPToolIndex,
		Vision:    NewMCPVisionGate(in.GW.ModelCatalog, in.Opts.Targets),
		Logger:    in.GW.Logger,
	})
	if appended == nil {
		// appendMCPResults only returns nil without appending when
		// payload["messages"] lost its []any shape — the tool results
		// (and the assistant tool_calls) were dropped entirely.
		in.GW.Logger.Warn("MCP loop: payload messages lost []any shape; tool results dropped before re-dispatch",
			"iteration", in.Iteration, "agent", in.Opts.AgentName, "mcp_calls", len(mcpCalls))
	}
	if rescanErr := rescanAppendedMessages(h, in, working, appended); rescanErr != nil {
		// The iteration's upstream response was successfully buffered but
		// never reaches the client from this path — still count its usage.
		RecordMCPUsage(h, in.GW, buf, in.Opts.AgentName)
		return false
	}
	return true
}

// rescanAppendedMessages runs the deterministic security subset of the
// interceptor chain (redact, entropy, tier-1 injection — NENYA-136) over the
// tool-result messages the MCP loop just appended, which never pass through
// the request-time chain. Interceptors mutate the appended maps in place, so
// the next marshaled payload carries the scrubbed content; the conversation
// prefix is untouched, keeping the upstream prompt-cache prefix stable. The
// injection interceptor runs with DisableEscalation: the ambiguous detection
// band falls back to the deterministic act verdict and never opens an LLM
// classifier hop mid-loop (the below-min pass band still passes). One
// request-path signal is intentionally dropped: entropy redaction's
// EntropyRedacted egress-screen trigger is recorded on the throwaway
// InterceptRequest and does not reach the response-path screen — the content
// itself is still scrubbed.
//
// Failure semantics mirror the request-path chain: a policy rejection
// (strict injection) or a strict operational failure is rendered to the
// client by writePipelineRejection and returned so the caller aborts the
// loop. handleRescanFailure renders the failure: a bare context error
// (client gone / loop deadline) replays the last good response or writes a
// timeout; real pipeline errors render via writePipelineRejection. A nil
// chain (gateway built without one) fails open — the pre-NENYA-136 behavior.
func rescanAppendedMessages(h Host, in mcpIterInput, working map[string]any, appended []map[string]any) error {
	chain := in.GW.MCPSecurityChain
	if chain == nil || len(appended) == 0 {
		return nil
	}
	// Agent: opts.Agent is a value with the documented zero-value-for-direct-
	// routes convention (forwardOptions). The injection interceptor treats a
	// zero Agent identically to nil (its overrides are all nil), so pointing
	// at the value is safe here.
	req := &pipeline.InterceptRequest{
		Payload:           working,
		Messages:          appended,
		AgentName:         in.Opts.AgentName,
		Agent:             &in.Opts.Agent,
		DisableEscalation: true,
	}
	if _, err := chain.Execute(in.MCPLoopCtx, req); err != nil {
		handleRescanFailure(h, in, err)
		return err
	}
	return nil
}

// handleRescanFailure renders a rescan failure to the client. A context
// cancellation/deadline is checked first — Execute wraps interceptor
// ctx.Err() in a StrictError, but it must not render as a 503; it mirrors
// the loop's own deadline path (replay the last good response or write a
// timeout) so the client never gets a silent empty response. Real pipeline
// rejections (strict injection) and strict operational faults render via
// writePipelineRejection.
func handleRescanFailure(h Host, in mcpIterInput, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		if in.LastBuf != nil && *in.LastBuf != nil {
			ReplayGuardedBufferedWithProvider(h, in.GW, in.W, *in.LastBuf, (*in.LastBuf).ProviderName, in.Opts.AgentName, in.Opts.SourceFormat, in.Opts.APIKey)
			return
		}
		h.WriteSSEError(in.GW, in.W, http.StatusRequestTimeout, "MCP loop deadline exceeded")
		return
	}
	h.WritePipelineRejection(in.GW, in.W, err)
}

// bufferedDispatch groups forwardBuffered's request-scoped parameters
// (AUD-60 / NENYA-314: thirteen positional parameters beyond the options
// threshold). gw, ctx, and r stay positional; everything that varies per
// dispatch is the struct.
type bufferedDispatch struct {
	Targets         []routing.UpstreamTarget
	Payload         map[string]interface{}
	Cooldown        time.Duration
	TokenCount      int
	AgentName       string
	MaxRetries      int
	APIKey          *config.ApiKey
	Canary          pipeline.CanarResult
	SourceFormat    string
	WindowCompacted bool
}

// ForwardBuffered dispatches a non-streaming request and buffers the whole
// SSE response (the buffered core of the MCP loop and the empty-200
// failover). The result carries raw bytes, extracted tool calls, usage, and
// the guard outcomes; callers replay or convert it per the client format.
func ForwardBuffered(h Host, gw *gateway.NenyaGateway,
	ctx context.Context,
	r *http.Request,
	d BufferedDispatch,
) (*BufferedSSE, error) {
	targets, payload := d.Targets, d.Payload
	cooldownDuration, tokenCount, agentName := d.Cooldown, d.TokenCount, d.AgentName
	maxRetries, apiKey, canary := d.MaxRetries, d.APIKey, d.Canary
	sourceFormat, windowCompacted := d.SourceFormat, d.WindowCompacted
	originalPayload, err := prepareOriginalPayload(gw, payload)
	if err != nil {
		return nil, err
	}

	attempt := 0
	sameTargetRetries := 0
	maxSameTargetRetries := maxRetries
	if maxSameTargetRetries <= 0 {
		maxSameTargetRetries = gw.Config.Governance.EffectiveMaxRetryAttempts()
	}
sweep:
	for i := 0; i < len(targets); i++ {
		target := targets[i]
		// max_retries bounds the retry budget; the forward sweep continues
		// while it does not matter, but once the budget is spent the
		// request ends rather than sweeping further.
		if maxRetries > 0 && attempt > maxRetries {
			gw.Logger.Warn("max retries reached in buffered mode",
				"attempt", attempt, "max", maxRetries, "agent", agentName)
			break
		}

		workingPayload := make(map[string]interface{})
		if err := json.Unmarshal(originalPayload, &workingPayload); err != nil {
			gw.Logger.Error("failed to unmarshal payload for target",
				"target", i+1, "total", len(targets), "err", err)
			continue
		}

		// The MCP buffered loop has no request-scoped audit flag: nil keeps
		// the per-attempt semantics there (each attempt is a distinct
		// logical dispatch in the multi-turn loop).
		action := h.PrepareAndSend(gw, r, DispatchParams{
			Idx:             i,
			Targets:         targets,
			Target:          target,
			Payload:         workingPayload,
			Cooldown:        cooldownDuration,
			TokenCount:      tokenCount,
			AgentName:       agentName,
			APIKey:          apiKey,
			Streaming:       false,
			SourceFormat:    sourceFormat,
			WindowCompacted: windowCompacted,
		})
		if action.Kind == actionError {
			attempt++
		}
		result, outcome := HandleBufferedAction(h, ctx, gw, i, targets, target, cooldownDuration, agentName, action, attempt, sameTargetRetries, maxRetries, canary)
		if result != nil {
			return result, nil
		}
		switch outcome.Step {
		case BufferedStepStop:
			break sweep
		case BufferedStepRetry:
			// Same-target retry when no later target remains dispatchable and
			// the same-target budget allows it (mirrors the chat loop: 413 is
			// failover-only, and backoff applies only before the repeat).
			if outcome.Status == http.StatusRequestEntityTooLarge ||
				hasLaterDispatchableTarget(gw, targets, i) ||
				sameTargetRetries >= maxSameTargetRetries {
				continue
			}
			sameTargetRetries++
			gw.Logger.Info("retrying same target (buffered)",
				"model", target.Model, "provider", target.Provider,
				"retry", sameTargetRetries, "max", maxSameTargetRetries)
			if !outcome.Waited {
				h.WaitWithCancel(ctx, h.CalculateBackoff(sameTargetRetries-1))
			}
			i-- // re-attempt this target
		}
	}

	return nil, fmt.Errorf("all %d upstream targets exhausted", len(targets))
}

// hasLaterDispatchableTarget reports whether any target after index i can be
// dispatched without hitting an open circuit breaker. Used by the MCP buffered
// sweep (the chat loop also excludes already-rate-limited pairs, which it
// tracks itself).
func hasLaterDispatchableTarget(gw *gateway.NenyaGateway, targets []routing.UpstreamTarget, i int) bool {
	for j := i + 1; j < len(targets); j++ {
		t := targets[j]
		if t.CoolKey != "" && gw != nil && !gw.AgentState.CB.Peek(t.CoolKey) {
			continue
		}
		return true
	}
	return false
}

func prepareOriginalPayload(gw *gateway.NenyaGateway, payload map[string]interface{}) ([]byte, error) {
	originalPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	if (gw.Config.Compaction.Enabled != nil && *gw.Config.Compaction.Enabled) && gw.Config.Compaction.JSONMinify != nil && *gw.Config.Compaction.JSONMinify {
		minified := bytes.NewBuffer(make([]byte, 0, len(originalPayload)))
		if err := json.Compact(minified, originalPayload); err == nil {
			originalPayload = minified.Bytes()
		}
	}
	return originalPayload, nil
}

// BufferedStep tells the MCP buffered sweep how to proceed after a target.
type BufferedStep int

const (
	// BufferedStepNext advances to the next target.
	BufferedStepNext BufferedStep = iota
	// BufferedStepRetry marks a transient failure eligible for a same-target
	// repeat when no later target remains.
	BufferedStepRetry
	// BufferedStepStop ends the sweep (terminal response or unretryable error).
	BufferedStepStop
)

// bufferedOutcome carries the buffered sweep's decision plus the metadata the
// caller needs to mirror the chat loop: the upstream status (for the 413
// failover-only rule) and whether the handler already slept.
type bufferedOutcome struct {
	Step   BufferedStep
	Status int
	Waited bool
}

// HandleBufferedAction resolves one buffered dispatch action: releases the
// concurrency slot, records cooldown/failure state, and either stops the
// retry sweep, waits out a cooldown retry, or fails over. Every path frees
// the per-model slot acquired by prepareAndSend (idempotent release).
func HandleBufferedAction(h Host, ctx context.Context, gw *gateway.NenyaGateway, idx int, targets []routing.UpstreamTarget, target routing.UpstreamTarget, cooldownDuration time.Duration, agentName string, action UpstreamAction, attempt, localRetries, maxRetries int, canary pipeline.CanarResult) (*BufferedSSE, BufferedOutcome) {
	// Every path must free the per-model concurrency slot acquired by
	// prepareAndSend (idempotent release).
	defer action.ReleaseSlot()
	switch action.Kind {
	case actionContinue:
		// A transient local failure (network) is eligible for a same-target
		// repeat; guard/skip actions are not. No sleep here: the caller backs
		// off only immediately before a repeat, so failover stays immediate.
		if action.Retryable {
			return nil, bufferedOutcome{Step: BufferedStepRetry}
		}
		return nil, bufferedOutcome{Step: BufferedStepNext}
	case actionError:
		status := action.Resp.StatusCode
		action.Body, _ = io.ReadAll(io.LimitReader(action.Resp.Body, pipeline.MaxErrorBodyBytes))
		_ = action.Resp.Body.Close()
		// Connection-scoped (NENYA-42): client gone — no state writes, no
		// further targets.
		if ctx.Err() != nil {
			action.Cancel()
			// Release the half-open probe slot consumed by the guard.
			gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
			gw.Logger.Info("MCP buffered: client context canceled during upstream error handling",
				"model", target.Model, "provider", target.Provider)
			return nil, bufferedOutcome{Step: BufferedStepStop, Status: status}
		}
		// Request-scoped via provider config (NENYA-42): the client's payload
		// is at fault — no cooldown/rotation state, no target sweep.
		if h.MatchRequestScopedError(gw, target.Provider, status, action.Body) != nil {
			action.Cancel()
			// No circuit outcome is recorded for a client-fault error; release
			// the half-open probe slot consumed by the guard.
			gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
			gw.Logger.Warn("MCP buffered: request-scoped error from provider, failing without rotation",
				"model", target.Model, "provider", target.Provider, "status", status)
			return nil, bufferedOutcome{Step: BufferedStepStop, Status: status}
		}
		gw.Logger.Debug("MCP buffered: upstream error",
			"target", idx+1, "status", status, "model", target.Model, "body_len", len(action.Body))
		// AUD-13 (NENYA-229): the quota flag is per call and the buffered MCP
		// path has no quota-aware exhaustion report, so it is ignored here —
		// exactly as before, minus the process-wide store that used to leak
		// this loop's quota state into the next chat request's classification.
		signal, retryDelay, _ := h.HandleUpstreamError(gw, idx, targets, target, cooldownDuration, agentName, action)
		action.Cancel()
		if signal == retrySignalDone {
			return nil, bufferedOutcome{Step: BufferedStepStop, Status: status}
		}
		if maxRetries > 0 && attempt > maxRetries {
			gw.Logger.Warn("max retries reached in buffered mode after error",
				"attempt", attempt, "max", maxRetries, "agent", agentName)
			return nil, bufferedOutcome{Step: BufferedStepStop, Status: status}
		}
		if retryDelay > 0 {
			gw.Logger.Info("retrying with parsed delay (buffered)",
				"model", target.Model, "delay_ms", retryDelay.Milliseconds())
			h.WaitWithCancel(ctx, retryDelay)
		} else {
			backoff := h.CalculateBackoff(attempt - 1)
			gw.Logger.Info("retrying with exponential backoff (buffered)",
				"model", target.Model, "attempt", attempt, "delay_ms", backoff.Milliseconds())
			h.WaitWithCancel(ctx, backoff)
		}
		if signal == retrySignalRetry {
			return nil, bufferedOutcome{Step: bufferedStepRetry, Status: status, Waited: true}
		}
		return nil, bufferedOutcome{Step: bufferedStepNext, Status: status, Waited: true}
	case actionStream:
		buf, err := HandleBufferedStream(h, ctx, action, target, gw, cooldownDuration, agentName, canary)
		if err != nil {
			// A buffering failure caused by the client going away is
			// connection-scoped (NENYA-42): release the probe slot instead of
			// charging the provider.
			if ctx.Err() != nil {
				gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
			} else {
				gw.AgentState.RecordFailure(target, cooldownDuration)
			}
			return nil, bufferedOutcome{Step: BufferedStepStop}
		}
		if buf.EmptyResponse {
			// The void is a failed attempt: continue the sweep instead of
			// replaying it (AUD-16 / NENYA-258). When no target remains the
			// loop's exhaustion path handles the request.
			return nil, bufferedOutcome{Step: BufferedStepNext}
		}
		return buf, bufferedOutcome{Step: BufferedStepStop}
	}
	return nil, bufferedOutcome{Step: BufferedStepStop}
}

// HandleBufferedStream consumes a streaming dispatch action into a buffered
// response (the stream-when-buffered path): applies the exfil/canary guards
// line-by-line and returns the accumulated buffer or the stream error.
func HandleBufferedStream(h Host, ctx context.Context, action UpstreamAction, target routing.UpstreamTarget, gw *gateway.NenyaGateway, cooldownDuration time.Duration, agentName string, canary pipeline.CanarResult) (*BufferedSSE, error) {
	defer action.Cancel()
	buf, err := bufferStreamResponse(ctx, action.Resp.Body, gw.Logger, target.Model, target)
	_ = action.Resp.Body.Close()
	bodyReadAt := time.Now()
	if err != nil {
		// The caller records the failure for this attempt; recording it here
		// too would charge one dispatch twice.
		return nil, fmt.Errorf("buffering response: %w", err)
	}
	// ExfilGuard (Phase 050): the MCP loop replays buffered SSE bytes
	// verbatim, bypassing the streaming filter pipeline — apply the URL
	// egress policy over the buffered frames before they reach callers.
	ApplyExfilToBufferedSSE(h, gw, buf, agentName)
	ApplyCanaryToBufferedSSE(h, gw, buf, agentName, canary)
	if buf.ExfilBlocked || buf.CanaryBlocked {
		// A policy block is not a provider failure: record no outcome (a
		// success would clear the failure streak and close a half-open
		// circuit on a response the gateway rejected), and release the probe
		// slot the dispatch consumed.
		gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
		return buf, nil
	}
	if buf.StreamError {
		// An in-stream error frame is a failed upstream attempt even though the
		// HTTP status was 200: charge the failure rather than a success
		// (NENYA-153).
		gw.AgentState.RecordFailure(target, cooldownDuration)
		return buf, nil
	}
	if buf.EmptyResponse {
		// AUD-16 (NENYA-258): an HTTP-200 that carried no SSE data frames is a
		// failed upstream attempt like any other — the chat loop fails over on
		// its empty results (streamResult{empty} → retryable). Pre-fix this
		// path recorded a breaker SUCCESS for the void, fired no
		// nenya_empty_responses_total counter, and replayed it to the client.
		gw.Metrics.RecordEmptyResponse(target.Model, target.Provider, "buffered")
		gw.AgentState.RecordFailure(target, cooldownDuration)
		gw.Logger.Warn("empty buffered response from upstream, trying next target",
			"model", target.Model, "provider", target.Provider)
		return buf, nil
	}
	// NENYA-153: a served slow call counts as a breaker failure instead of a
	// success, so a provider that answers-but-takes-minutes stops pooling
	// traffic. Only a response the gateway actually serves records an outcome.
	h.RecordBufferedOutcome(gw, target, agentName, action.DispatchAt, bodyReadAt, cooldownDuration)
	return buf, nil
}

// applyExfilToBufferedSSE runs the egress guard over each model-text
// frame of a buffered SSE response, rewriting stripped deltas in place.
// A block verdict truncates the buffer to the frames before the
// violation; callers detect it via buf.ExfilBlocked and terminate with
// the structured block payload instead of replaying.
func ApplyExfilToBufferedSSE(h Host, gw *gateway.NenyaGateway, buf *BufferedSSE, agentName string) {
	guard := h.ExfilGuardFor(gw, agentName)
	if guard == nil || buf == nil || len(buf.RawBytes) == 0 {
		return
	}
	var out []byte
	blocked := false
	changed := false
	var lastContent map[string]interface{}
	for _, line := range strings.Split(string(buf.RawBytes), "\n") {
		if blocked && !strings.Contains(line, "usage") {
			// After a block verdict only bookkeeping frames (usage) are
			// kept; content frames are dropped with the violation.
			continue
		}
		next, lineBlocked, lineChanged, lineContent := GuardBufferedLine(h, guard, line)
		blocked = blocked || lineBlocked
		changed = changed || lineChanged
		if lineContent != nil {
			lastContent = lineContent
		}
		out = append(out, next...)
	}
	if !blocked {
		// Stream end: the guard's held tail is final — drain it as a
		// synthetic frame in the last content frame's shape (NENYA-205),
		// so a held tail is withheld from the replay neither partially
		// nor entirely.
		var flushed bool
		out, flushed = appendExfilFlushFrame(out, guard, lastContent)
		changed = changed || flushed
	}
	if changed {
		buf.RawBytes = out
		buf.ExfilBlocked = blocked
	}
}

// applyCanaryToBufferedSSE scans the model-text frames of a buffered
// response for the request canary through ONE persistent watcher, so a
// token straddling delta frames is detected at completion (per-frame
// scans would miss it). Block truncates the buffer at the completing
// frame (canaryBlocked); log scrubs the marker best-effort per frame
// (a straddled token is detected but its fragments may remain) and
// continues.
func ApplyCanaryToBufferedSSE(h Host, gw *gateway.NenyaGateway, buf *BufferedSSE, agentName string, canary pipeline.CanarResult) {
	if canary.Token == "" || buf == nil || len(buf.RawBytes) == 0 {
		return
	}
	watcher := stream.NewCanaryWatcher(canary.Token, canary.Action, "buffered", gw.Metrics, gw.Logger.With("agent", agentName))
	var out []byte
	blocked := false
	changed := false
	for _, line := range strings.Split(string(buf.RawBytes), "\n") {
		if blocked && !strings.Contains(line, "usage") {
			// After a block verdict only bookkeeping frames (usage) are
			// kept, mirroring the egress guard; content frames are
			// dropped with the violation.
			continue
		}
		next, lineBlocked, lineChanged := CanaryBufferedLine(h, watcher, canary, line)
		blocked = blocked || lineBlocked
		changed = changed || lineChanged
		out = append(out, next...)
	}
	if changed {
		buf.RawBytes = out
		buf.CanaryBlocked = blocked
	}
}

// canaryBufferedLine runs the canary watcher over one buffered SSE
// line: block verdicts report truncation; log verdicts scrub the marker
// sentence from frames after the trip.
func CanaryBufferedLine(h Host, watcher *stream.CanaryWatcher, canary pipeline.CanarResult, line string) (string, bool, bool) {
	// Tolerant field recognition (NENYA-197): a no-space data line must be
	// scanned, or the tripwire silently skips the payload it exists to catch.
	value, isData := stream.SSEDataField([]byte(line))
	if !isData {
		return line + "\n", false, false
	}
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" || trimmed == stream.DonePayload {
		return line + "\n", false, false
	}
	parsed := stream.ParseSSEChunk([]byte(trimmed))
	if parsed == nil {
		return line + "\n", false, false
	}
	content := stream.ExtractExfilContent(parsed)
	if content == "" {
		return line + "\n", false, false
	}
	_, action, _ := watcher.FilterContent(content)
	if action == stream.ActionBlock {
		// The watcher emits the one-time detection warning (with agent
		// context).
		return "", true, true
	}
	if watcher.Tripped && canary.Action == config.CanaryActionLog {
		// Log mode: scrub the marker sentence (falling back to the bare
		// token) from frames carrying it.
		cleaned := strings.ReplaceAll(content, pipeline.CanaryMarker(canary.Token), "")
		cleaned = strings.ReplaceAll(cleaned, canary.Token, "")
		if cleaned != content {
			_ = stream.SetExfilContent(parsed, cleaned)
			reencoded, err := json.Marshal(parsed)
			if err != nil {
				// Log mode is fail-open: pass the original line through
				// rather than dropping content.
				return line + "\n", false, false
			}
			return "data: " + string(reencoded) + "\n", false, true
		}
	}
	return line + "\n", false, false
}

// appendExfilFlushFrame drains the guard's held tail at stream end and
// inserts the synthetic frame ahead of the terminal [DONE] (no content
// frame may follow the sentinel). Returns the buffer and whether it
// changed.
func appendExfilFlushFrame(out []byte, guard *stream.ExfilGuard, lastContent map[string]interface{}) ([]byte, bool) {
	frame := stream.FlushExfilHeldFrame(guard, lastContent)
	if frame == nil {
		return out, false
	}
	// The frame ends with one newline; close the event with a second so
	// it cannot merge with the [DONE] line into a single SSE event (a
	// strict parser would then never see the sentinel).
	frame = append(frame, '\n')
	if idx := bytes.LastIndex(out, []byte("\ndata: [DONE]")); idx >= 0 {
		// Anchor one line back: an occurrence of the sentinel literal
		// inside a content frame must not redirect the insertion.
		idx++
		return append(out[:idx], append(frame, out[idx:]...)...), true
	}
	return append(out, frame...), true
}

// guardBufferedLine runs the guard over one buffered SSE line, returning
// the (possibly rewritten) line, whether it triggered a block, whether
// it was rewritten, and the parsed chunk when it carried model text
// (the terminal flush mimics its shape).
func GuardBufferedLine(h Host, guard *stream.ExfilGuard, line string) (string, bool, bool, map[string]interface{}) {
	// Tolerant field recognition (NENYA-197): see canaryBufferedLine.
	value, isData := stream.SSEDataField([]byte(line))
	if !isData {
		return line + "\n", false, false, nil
	}
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" || trimmed == stream.DonePayload {
		return line + "\n", false, false, nil
	}
	parsed := stream.ParseSSEChunk([]byte(trimmed))
	if parsed == nil {
		return line + "\n", false, false, nil
	}
	content := stream.ExtractExfilContent(parsed)
	if content == "" {
		return line + "\n", false, false, nil
	}
	rewritten, action, _ := guard.FilterContent(content)
	switch action {
	case stream.ActionBlock:
		return "", true, true, nil
	case stream.ActionRedact:
		_ = stream.SetExfilContent(parsed, rewritten)
		reencoded, err := json.Marshal(parsed)
		if err != nil {
			// Fail-safe: drop the line rather than leak the unredacted
			// original when re-encoding fails.
			return "", false, true, nil
		}
		return "data: " + string(reencoded) + "\n", false, true, parsed
	default:
		// Pass with a (possibly) shortened emit: the guard withheld a
		// whitespace-free tail (NENYA-205); its return is the
		// client-visible content for this frame and the held remainder
		// is appended by the Flush drain after the loop.
		if rewritten != content {
			_ = stream.SetExfilContent(parsed, rewritten)
			reencoded, err := json.Marshal(parsed)
			if err != nil {
				// Fail-safe: drop the frame rather than leak the held tail.
				return "", false, true, nil
			}
			return "data: " + string(reencoded) + "\n", false, true, parsed
		}
		return line + "\n", false, false, parsed
	}
}

// replayGuardedBuffered replays a buffered MCP response through the
// egress-guard verdict: a buffer truncated by the guard gets the
// structured block payload instead of the violating frames.
// lastBufProviderName extracts the producing provider from a buffered
// response, nil-safe (NENYA-155 replay telemetry).
func lastBufProviderName(buf *bufferedSSE) string {
	if buf == nil {
		return ""
	}
	return buf.ProviderName
}

// replayGuardedBufferedWithProvider threads the target's provider name so a
// client stall on the replay write is telemetered with the real label
// (NENYA-155); the provider-less wrapper keeps the older call sites honest.
func ReplayGuardedBufferedWithProvider(h Host, gw *gateway.NenyaGateway, w http.ResponseWriter, buf *BufferedSSE, providerName string, agentName string, sourceFormat string, apiKey *config.ApiKey) {
	if buf != nil && buf.ExfilBlocked {
		gw.Logger.Warn("MCP loop: response blocked by exfil guard", "agent", agentName)
		h.WriteExfilBlockedSSE(gw, w)
		return
	}
	if buf != nil && buf.CanaryBlocked {
		gw.Logger.Warn("MCP loop: response blocked by canary tripwire", "agent", agentName)
		h.WriteExfilDetectedSSE(gw, w)
		return
	}
	// Issue 723f3db1: the replay must speak the CLIENT's wire format — a
	// native-frame replay reached mismatched clients. Converted replayBufferedResponse
	// mutates nothing shared: the conversion allocates a fresh buffer.
	if buf != nil {
		if converted := ConvertBufferedForReplay(h, gw, buf, sourceFormat, apiKey); !bytes.Equal(converted, buf.RawBytes) {
			buf = &bufferedSSE{RawBytes: converted, ServingTarget: buf.ServingTarget}
		}
	}
	replayBufferedResponse(h, w, buf, gw, providerName, gw.Logger)
}

func detectRequestCapabilities(payload map[string]interface{}) routing.RequestCapabilities {
	var caps routing.RequestCapabilities

	if tools, ok := payload["tools"].([]interface{}); ok && len(tools) > 0 {
		caps.HasToolCalls = true
	}

	messages, ok := payload["messages"].([]interface{})
	if !ok {
		return caps
	}
	for _, msg := range messages {
		if inspectMessageCaps(msg, &caps) {
			break
		}
	}

	return caps
}

// inspectMessageCaps inspects a message object and updates the RequestCapabilities struct.
// Returns true if both HasVision and HasReasoning capabilities are detected.
func inspectMessageCaps(msg any, caps *routing.RequestCapabilities) bool {
	m, ok := msg.(map[string]interface{})
	if !ok {
		return false
	}
	content := m["content"]
	if arr, ok := content.([]interface{}); ok && len(arr) > 0 {
		caps.HasContentArr = true
		checkContentArrayForVision(arr, caps)
	}
	if reasoning, ok := m["reasoning"].(map[string]interface{}); ok && len(reasoning) > 0 {
		caps.HasReasoning = true
	}
	return caps.HasVision && caps.HasReasoning
}

// checkContentArrayForVision scans a content array for vision content (image_url).
func checkContentArrayForVision(arr []interface{}, caps *routing.RequestCapabilities) {
	for _, part := range arr {
		p, ok := part.(map[string]interface{})
		if !ok {
			continue
		}
		if t, ok := p["type"].(string); ok && t == "image_url" {
			caps.HasVision = true
			return
		}
	}
}
