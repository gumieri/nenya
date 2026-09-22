package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/mcp"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
	"github.com/nenya/internal/util"
)

// injectMCPTools injects discovered MCP tools into the payload for the
// request's agent. Reads AgentName (logging identity) and Agent (resolved
// config) from the same chatRequest, so they cannot diverge.
func (p *Proxy) injectMCPTools(gw *gateway.NenyaGateway, payload map[string]interface{}, req *chatRequest) {
	agent := req.Agent
	if agent.MCP == nil || len(agent.MCP.Servers) == 0 {
		return
	}
	agentName := req.AgentName

	gw.Logger.Info("MCP injection starting",
		"servers", agent.MCP.Servers, "agent", agentName)

	var toolNames []string
	for _, serverName := range agent.MCP.Servers {
		client, ok := gw.MCPClients[serverName]
		if !ok || !client.Ready() {
			gw.Logger.Warn("MCP server not available, skipping tool injection",
				"server", serverName, "agent", agentName)
			continue
		}

		tools := client.ListTools()
		if len(tools) == 0 {
			gw.Logger.Warn("MCP server returned no tools",
				"server", serverName, "agent", agentName)
			continue
		}
		openaiTools := mcp.MCPToolsToOpenAI(serverName, tools)
		applyToolDescriptionSpotlight(openaiTools, spotlightSettingsFor(&gw.Config, agent), gw.Metrics)

		existing, ok := payload["tools"].([]interface{})
		if !ok {
			existing = []interface{}{}
		}

		for _, t := range openaiTools {
			existing = append(existing, t)
			if fn, ok := t["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok {
					toolNames = append(toolNames, name)
				}
			}
		}

		payload["tools"] = existing
		gw.Logger.Debug("MCP tools injected",
			"server", serverName, "tools", len(tools), "agent", agentName)
	}

	if len(toolNames) > 0 {
		if _, has := payload["tool_choice"]; !has {
			payload["tool_choice"] = "auto"
			gw.Logger.Info("MCP tool_choice auto injected",
				"tools_count", len(toolNames), "agent", agentName)
		}
		p.injectMCPSystemPrompt(gw, payload, toolNames, spotlightSettingsFor(&gw.Config, agent))
	} else {
		gw.Logger.Warn("MCP: no tools injected for agent",
			"agent", agentName, "servers", agent.MCP.Servers)
	}
}

func (p *Proxy) injectMCPSystemPrompt(gw *gateway.NenyaGateway, payload map[string]interface{}, toolNames []string, settings pipeline.SpotlightSettings) {
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

func (p *Proxy) discoverToolByPrefix(gw *gateway.NenyaGateway, serverName, prefix string) string {
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
func (p *Proxy) injectAutoSearch(gw *gateway.NenyaGateway, ctx context.Context, payload map[string]interface{}, messages []interface{}, req *chatRequest) {
	agent := req.Agent
	agentName := req.AgentName
	if agent.MCP == nil || !agent.MCP.AutoSearch {
		return
	}

	query, _ := p.extractAutoSearchQuery(messages)
	if query == "" {
		return
	}

	query = p.redactQuery(gw, query)

	for _, serverName := range agent.MCP.Servers {
		if !p.canPerformAutoSearch(gw, serverName) {
			continue
		}

		toolName := p.resolveSearchTool(gw, serverName, agent.MCP.SearchTool, agentName)
		if toolName == "" {
			continue
		}

		if result := p.executeAutoSearch(gw, ctx, serverName, toolName, query, agentName); result != nil {
			p.injectAutoSearchContext(gw, autoSearchContextOpts{
				payload:    payload,
				messages:   messages,
				serverName: serverName,
				result:     result,
				toolName:   toolName,
				agentName:  agentName,
				settings:   spotlightSettingsFor(&gw.Config, agent),
			})
			break
		}
	}
}

func (p *Proxy) extractAutoSearchQuery(messages []interface{}) (string, map[string]interface{}) {
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

func (p *Proxy) redactQuery(gw *gateway.NenyaGateway, query string) string {
	query = pipeline.RedactSecrets(query, (gw.Config.Bouncer.Enabled != nil && *gw.Config.Bouncer.Enabled), gw.SecretPatterns, gw.Config.Bouncer.RedactionLabel)
	if gw.EntropyFilter != nil {
		query = gw.EntropyFilter.RedactHighEntropy(query, gw.Config.Bouncer.RedactionLabel)
	}
	return query
}

func (p *Proxy) canPerformAutoSearch(gw *gateway.NenyaGateway, serverName string) bool {
	client, ok := gw.MCPClients[serverName]
	return ok && client.Ready()
}

func (p *Proxy) resolveSearchTool(gw *gateway.NenyaGateway, serverName, configuredTool, agentName string) string {
	if configuredTool != "" {
		return configuredTool
	}
	toolName := p.discoverToolByPrefix(gw, serverName, "search")
	if toolName == "" {
		gw.Logger.Warn("MCP auto-search: no 'search' tool found on server",
			"server", serverName, "agent", agentName)
	}
	return toolName
}

type autoSearchResult struct {
	text      string
	toolName  string
	duration  time.Duration
	server    string
	agentName string
}

func (p *Proxy) executeAutoSearch(gw *gateway.NenyaGateway, ctx context.Context, serverName, toolName, query, agentName string) *autoSearchResult {
	start := time.Now()
	result, err := p.mcpClientCallTool(gw, ctx, serverName, toolName, query)
	duration := time.Since(start)

	if err != nil {
		gw.Logger.Warn("MCP auto-search failed, proceeding without",
			"server", serverName, "agent", agentName, "err", err,
			"duration_ms", duration.Milliseconds())
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

	return &autoSearchResult{
		text:      p.redactSearchResult(gw, result.Text()),
		toolName:  toolName,
		duration:  duration,
		server:    serverName,
		agentName: agentName,
	}
}

func (p *Proxy) mcpClientCallTool(gw *gateway.NenyaGateway, ctx context.Context, serverName, toolName, query string) (*mcp.CallToolResult, error) {
	client, ok := gw.MCPClients[serverName]
	if !ok {
		return nil, fmt.Errorf("MCP client not found")
	}
	return client.CallTool(ctx, toolName, map[string]any{
		"query": query,
		"limit": 5,
	})
}

func (p *Proxy) redactSearchResult(gw *gateway.NenyaGateway, resultText string) string {
	resultText = pipeline.RedactSecrets(resultText, (gw.Config.Bouncer.Enabled != nil && *gw.Config.Bouncer.Enabled), gw.SecretPatterns, gw.Config.Bouncer.RedactionLabel)
	if gw.EntropyFilter != nil {
		resultText = gw.EntropyFilter.RedactHighEntropy(resultText, gw.Config.Bouncer.RedactionLabel)
	}
	return resultText
}

// autoSearchContextOpts groups the parameters for injecting auto-search
// memory context (AGENTS.md §11 parameter grouping).
type autoSearchContextOpts struct {
	payload    map[string]interface{}
	messages   []interface{}
	serverName string
	result     *autoSearchResult
	toolName   string
	agentName  string
	settings   pipeline.SpotlightSettings
}

func (p *Proxy) injectAutoSearchContext(gw *gateway.NenyaGateway, opts autoSearchContextOpts) {
	contextStr := fmt.Sprintf("[Memory context from %s]\n%s", opts.serverName, opts.result.text)
	contextStr = opts.settings.ApplyDelimitersOnly(contextStr, pipeline.SpotlightSourceMemory(opts.serverName))
	memoryMsg := map[string]interface{}{
		"role":    "system",
		"content": contextStr,
	}

	updated := make([]interface{}, 0, util.AddCap(1, len(opts.messages)))
	updated = append(updated, opts.messages[:len(opts.messages)-1]...)
	updated = append(updated, memoryMsg)
	updated = append(updated, opts.messages[len(opts.messages)-1:]...)
	opts.payload["messages"] = updated

	if opts.settings.Enabled {
		gw.Metrics.RecordSpotlighted("memory:" + opts.serverName)
	}
	gw.Logger.Debug("MCP auto-search context injected",
		"server", opts.serverName, "agent", opts.agentName,
		"tool", opts.toolName,
		"duration_ms", opts.result.duration.Milliseconds(),
		"result_len", len(opts.result.text))
	gw.Metrics.RecordMCPAutoSearch(opts.serverName, opts.agentName, true, nil)
}

func (p *Proxy) forwardToUpstreamWithMCP(gw *gateway.NenyaGateway,
	w http.ResponseWriter,
	r *http.Request,
	opts forwardOptions) {
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
		writeSSEError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	var lastBuf *bufferedSSE
	loopStart := time.Now()
	totalToolCalls := 0
	actualIter := 0

	mcpLoopCtx, mcpLoopCancel := context.WithTimeout(r.Context(), mcpLoopMaxDuration)
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
			gw:              gw,
			mcpLoopCtx:      mcpLoopCtx,
			w:               w,
			r:               r,
			opts:            opts,
			iteration:       iteration,
			originalPayload: &originalPayload,
			lastBuf:         &lastBuf,
			actualIter:      &actualIter,
			totalToolCalls:  &totalToolCalls,
		}
		switch p.mcpIteration(in) {
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
		p.replayGuardedBuffered(gw, w, lastBuf, opts.AgentName)
		p.recordMCPUsage(gw, lastBuf, opts.AgentName)
		return
	}

	writeStructuredError(w, http.StatusInternalServerError, infra.ErrorKindInternal, "MCP loop ended without response")
}

const (
	mcpIterContinue = iota
	mcpIterReturn
	mcpIterStop
)

type mcpIterInput struct {
	gw              *gateway.NenyaGateway
	mcpLoopCtx      context.Context
	w               http.ResponseWriter
	r               *http.Request
	opts            forwardOptions
	iteration       int
	originalPayload *[]byte
	lastBuf         **bufferedSSE
	actualIter      *int
	totalToolCalls  *int
}

// handleMCPBufferOutcome resolves a buffered upstream outcome that
// terminates the iteration without further processing: an exfil-blocked
// buffer (block payload written) or an upstream failure (last response
// replayed / gateway error written). Returns true when the caller must
// return immediately.
func (p *Proxy) handleMCPBufferOutcome(in mcpIterInput, buf *bufferedSSE, err error) bool {
	if err == nil && buf != nil && buf.exfilBlocked {
		// The guard truncated this turn's buffer: terminate the whole
		// loop with the block payload — continuing would execute tool
		// calls or fold reasoning derived from violating content.
		in.gw.Logger.Warn("MCP loop: response blocked by exfil guard",
			"iteration", in.iteration, "agent", in.opts.AgentName)
		p.writeExfilBlockedSSE(in.gw, in.w)
		return true
	}
	return false
}

func (p *Proxy) mcpIteration(in mcpIterInput) int {
	select {
	case <-in.mcpLoopCtx.Done():
		in.gw.Logger.Warn("MCP loop deadline exceeded", "agent", in.opts.AgentName, "iterations", *in.actualIter)
		if *in.lastBuf != nil {
			p.replayGuardedBuffered(in.gw, in.w, *in.lastBuf, in.opts.AgentName)
		} else {
			writeSSEError(in.w, http.StatusRequestTimeout, "MCP loop deadline exceeded")
		}
		return mcpIterReturn
	default:
	}

	in.gw.Metrics.RecordMCPLoopIteration(in.opts.AgentName)
	(*in.actualIter)++

	working := make(map[string]interface{})
	if err := json.Unmarshal(*in.originalPayload, &working); err != nil {
		in.gw.Logger.Error("failed to unmarshal payload for MCP iteration", "err", err)
		return mcpIterStop
	}

	if in.iteration == 0 {
		working = in.opts.Payload
	}

	buf, err := p.forwardBuffered(in.gw, in.mcpLoopCtx, in.r, in.opts.Targets, working, in.opts.Cooldown, in.opts.TokenCount, in.opts.AgentName, in.opts.MaxRetries, in.opts.ApiKey)
	if handled := p.handleMCPBufferOutcome(in, buf, err); handled {
		return mcpIterReturn
	}
	if err != nil {
		in.gw.Logger.Warn("MCP loop: upstream failed, streaming last response",
			"iteration", in.iteration, "err", err)
		if *in.lastBuf != nil {
			p.replayGuardedBuffered(in.gw, in.w, *in.lastBuf, in.opts.AgentName)
			return mcpIterReturn
		}
		writeSSEError(in.w, http.StatusBadGateway, "All upstream providers failed")
		return mcpIterReturn
	}

	allCalls := buf.toolCalls
	if len(allCalls) == 0 {
		in.gw.Logger.Debug("MCP loop: content-only response, replaying",
			"has_content", buf.hasContent,
			"finish_reason", buf.finishReason,
			"raw_bytes_len", len(buf.rawBytes))
		p.replayGuardedBuffered(in.gw, in.w, buf, in.opts.AgentName)
		p.recordMCPUsage(in.gw, buf, in.opts.AgentName)
		return mcpIterReturn
	}

	mcpCalls, nonMcpCalls := partitionMCPToolCalls(allCalls, in.gw.MCPToolIndex)
	*in.totalToolCalls += len(mcpCalls)

	if len(mcpCalls) > 0 {
		in.gw.Logger.Info("MCP tool calls intercepted",
			"mcp_calls", len(mcpCalls),
			"non_mcp_calls", len(nonMcpCalls),
			"iteration", in.iteration+1,
			"agent", in.opts.AgentName)

		results := executeMCPCalls(in.mcpLoopCtx, mcpCalls, in.gw, in.opts.AgentName)
		mcpAssistantMsg := map[string]any{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": buildOpenAIToolCalls(mcpCalls),
		}
		if buf.reasoningContent != "" {
			mcpAssistantMsg["reasoning_content"] = buf.reasoningContent
		}
		appendMCPResults(working, mcpCalls, results, mcpAssistantMsg, spotlightSettingsFor(&in.gw.Config, in.opts.Agent), in.gw.Metrics)

		updatedPayload, err := json.Marshal(working)
		if err != nil {
			in.gw.Logger.Error("failed to marshal updated payload for MCP loop", "err", err)
			p.replayGuardedBuffered(in.gw, in.w, buf, in.opts.AgentName)
			return mcpIterReturn
		}
		*in.originalPayload = updatedPayload
	}

	if len(mcpCalls) == 0 && len(nonMcpCalls) > 0 {
		in.gw.Logger.Debug("MCP loop: non-MCP tool calls only, replaying",
			"non_mcp_calls", len(nonMcpCalls),
			"raw_bytes_len", len(buf.rawBytes))
		p.replayGuardedBuffered(in.gw, in.w, buf, in.opts.AgentName)
		p.recordMCPUsage(in.gw, buf, in.opts.AgentName)
		return mcpIterReturn
	}

	*in.lastBuf = buf
	return mcpIterContinue
}

func (p *Proxy) forwardBuffered(gw *gateway.NenyaGateway,
	ctx context.Context,
	r *http.Request,
	targets []routing.UpstreamTarget,
	payload map[string]interface{},
	cooldownDuration time.Duration,
	tokenCount int,
	agentName string,
	maxRetries int,
	apiKey *config.ApiKey,
) (*bufferedSSE, error) {
	originalPayload, err := prepareOriginalPayload(gw, payload)
	if err != nil {
		return nil, err
	}

	attempt := 0
	for i, target := range targets {
		if maxRetries > 0 && attempt >= maxRetries {
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

		action := p.prepareAndSend(gw, r, i, targets, target, workingPayload, cooldownDuration, tokenCount, agentName, apiKey, false)
		result, shouldContinue := p.handleBufferedAction(ctx, gw, i, targets, target, cooldownDuration, agentName, action, attempt, maxRetries)
		if result != nil {
			return result, nil
		}
		if !shouldContinue {
			break
		}
	}

	return nil, fmt.Errorf("all %d upstream targets exhausted", len(targets))
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

func (p *Proxy) handleBufferedAction(ctx context.Context, gw *gateway.NenyaGateway, idx int, targets []routing.UpstreamTarget, target routing.UpstreamTarget, cooldownDuration time.Duration, agentName string, action upstreamAction, attempt, maxRetries int) (*bufferedSSE, bool) {
	switch action.kind {
	case actionContinue:
		return nil, true
	case actionError:
		attempt++
		action.body, _ = io.ReadAll(io.LimitReader(action.resp.Body, pipeline.MaxErrorBodyBytes))
		_ = action.resp.Body.Close()
		// Connection-scoped (NENYA-42): client gone — no state writes, no
		// further targets.
		if ctx.Err() != nil {
			action.cancel()
			gw.Logger.Info("MCP buffered: client context canceled during upstream error handling",
				"model", target.Model, "provider", target.Provider)
			return nil, false
		}
		// Request-scoped via provider config (NENYA-42): the client's payload
		// is at fault — no cooldown/rotation state, no target sweep.
		if p.matchRequestScopedError(gw, target.Provider, action.resp.StatusCode, action.body) != nil {
			action.cancel()
			gw.Logger.Warn("MCP buffered: request-scoped error from provider, failing without rotation",
				"model", target.Model, "provider", target.Provider, "status", action.resp.StatusCode)
			return nil, false
		}
		gw.Logger.Debug("MCP buffered: upstream error",
			"target", idx+1,
			"status", action.resp.StatusCode,
			"model", target.Model,
			"body_len", len(action.body))
		shouldRetry, retryDelay := p.handleUpstreamError(gw, idx, targets, target, cooldownDuration, agentName, action)
		action.cancel()
		if shouldRetry {
			if maxRetries > 0 && attempt >= maxRetries {
				gw.Logger.Warn("max retries reached in buffered mode after error",
					"attempt", attempt, "max", maxRetries, "agent", agentName)
				return nil, false
			}
			if retryDelay > 0 {
				gw.Logger.Info("retrying with parsed delay (buffered)",
					"model", target.Model, "delay_ms", retryDelay.Milliseconds())
				waitWithCancel(ctx, retryDelay)
			} else {
				backoff := calculateBackoff(attempt - 1)
				gw.Logger.Info("retrying with exponential backoff (buffered)",
					"model", target.Model, "attempt", attempt, "delay_ms", backoff.Milliseconds())
				waitWithCancel(ctx, backoff)
			}
			return nil, true
		}
		return nil, false
	case actionStream:
		buf, err := p.handleBufferedStream(ctx, action, target, gw, cooldownDuration, agentName)
		if err != nil {
			gw.AgentState.RecordFailure(target, cooldownDuration)
		}
		return buf, false
	}
	return nil, false
}

func (p *Proxy) handleBufferedStream(ctx context.Context, action upstreamAction, target routing.UpstreamTarget, gw *gateway.NenyaGateway, cooldownDuration time.Duration, agentName string) (*bufferedSSE, error) {
	defer action.cancel()
	buf, err := bufferStreamResponse(ctx, action.resp.Body, gw.Logger)
	_ = action.resp.Body.Close()
	if err != nil {
		gw.AgentState.RecordFailure(target, cooldownDuration)
		return nil, fmt.Errorf("buffering response: %w", err)
	}
	gw.AgentState.RecordSuccess(target.CoolKey)
	// ExfilGuard (Phase 050): the MCP loop replays buffered SSE bytes
	// verbatim, bypassing the streaming filter pipeline — apply the URL
	// egress policy over the buffered frames before they reach callers.
	p.applyExfilToBufferedSSE(gw, buf, agentName)
	return buf, nil
}

// applyExfilToBufferedSSE runs the egress guard over each model-text
// frame of a buffered SSE response, rewriting stripped deltas in place.
// A block verdict truncates the buffer to the frames before the
// violation; callers detect it via buf.exfilBlocked and terminate with
// the structured block payload instead of replaying.
func (p *Proxy) applyExfilToBufferedSSE(gw *gateway.NenyaGateway, buf *bufferedSSE, agentName string) {
	guard := exfilGuardFor(gw, agentName)
	if guard == nil || buf == nil || len(buf.rawBytes) == 0 {
		return
	}
	var out []byte
	blocked := false
	changed := false
	for _, line := range strings.Split(string(buf.rawBytes), "\n") {
		if blocked && !strings.Contains(line, "usage") {
			// After a block verdict only bookkeeping frames (usage) are
			// kept; content frames are dropped with the violation.
			continue
		}
		next, lineBlocked, lineChanged := p.guardBufferedLine(guard, line)
		blocked = blocked || lineBlocked
		changed = changed || lineChanged
		out = append(out, next...)
	}
	if changed {
		buf.rawBytes = out
		buf.exfilBlocked = blocked
	}
}

// guardBufferedLine runs the guard over one buffered SSE line, returning
// the (possibly rewritten) line, whether it triggered a block, and
// whether it was rewritten.
func (p *Proxy) guardBufferedLine(guard *stream.ExfilGuard, line string) (string, bool, bool) {
	trimmed := strings.TrimPrefix(line, "data: ")
	if line == trimmed || strings.TrimSpace(trimmed) == "" || strings.TrimSpace(trimmed) == "[DONE]" {
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
	rewritten, action, _ := guard.FilterContent(content)
	switch action {
	case stream.ActionBlock:
		return "", true, true
	case stream.ActionRedact:
		_ = stream.SetExfilContent(parsed, rewritten)
		reencoded, err := json.Marshal(parsed)
		if err != nil {
			// Fail-safe: drop the line rather than leak the unredacted
			// original when re-encoding fails.
			return "", false, true
		}
		return "data: " + string(reencoded) + "\n", false, true
	default:
		return line + "\n", false, false
	}
}

// replayGuardedBuffered replays a buffered MCP response through the
// egress-guard verdict: a buffer truncated by the guard gets the
// structured block payload instead of the violating frames.
func (p *Proxy) replayGuardedBuffered(gw *gateway.NenyaGateway, w http.ResponseWriter, buf *bufferedSSE, agentName string) {
	if buf != nil && buf.exfilBlocked {
		gw.Logger.Warn("MCP loop: response blocked by exfil guard", "agent", agentName)
		p.writeExfilBlockedSSE(gw, w)
		return
	}
	replayBufferedResponse(w, buf, gw.Logger)
}

func (p *Proxy) recordMCPUsage(gw *gateway.NenyaGateway, buf *bufferedSSE, agentName string) {
	if buf == nil || gw == nil || agentName == "" {
		return
	}
	var lastData map[string]interface{}
	for _, line := range strings.Split(string(buf.rawBytes), "\n") {
		line = strings.TrimPrefix(line, "data: ")
		line = strings.TrimSpace(line)
		if line == "" || line == "[DONE]" {
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		if _, hasUsage := chunk["usage"]; hasUsage {
			lastData = chunk
		}
	}
	if lastData == nil {
		return
	}
	usage, ok := lastData["usage"].(map[string]interface{})
	if !ok {
		return
	}
	model := buf.model
	if model == "" {
		if m, ok := lastData["model"].(string); ok {
			model = m
		}
	}
	if model == "" {
		return
	}
	gw.Logger.Debug("MCP loop usage recorded",
		"agent", agentName, "model", model,
		"usage", usage)
	recordChatUsage(gw, model, usage)
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

// handleNonStreamingResponse buffers the full upstream response and returns it as a complete JSON object.
