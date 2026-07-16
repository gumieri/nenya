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

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/mcp"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/util"
)

func (p *Proxy) injectMCPTools(gw *gateway.NenyaGateway, payload map[string]interface{}, agentName string) {
	if agentName == "" {
		return
	}
	agent, ok := gw.Config.Agents[agentName]
	if !ok || agent.MCP == nil || len(agent.MCP.Servers) == 0 {
		return
	}

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
		p.injectMCPSystemPrompt(gw, payload, toolNames)
	} else {
		gw.Logger.Warn("MCP: no tools injected for agent",
			"agent", agentName, "servers", agent.MCP.Servers)
	}
}

func (p *Proxy) injectMCPSystemPrompt(gw *gateway.NenyaGateway, payload map[string]interface{}, toolNames []string) {
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

	mcpMsg := map[string]interface{}{
		"role":    "system",
		"content": prompt,
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

func (p *Proxy) injectAutoSearch(gw *gateway.NenyaGateway, ctx context.Context, payload map[string]interface{}, messages []interface{}, agentName string) {
	if agentName == "" {
		return
	}
	agent, ok := gw.Config.Agents[agentName]
	if !ok || agent.MCP == nil || !agent.MCP.AutoSearch {
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
			p.injectAutoSearchContext(gw, payload, messages, serverName, result, toolName, agentName)
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

func (p *Proxy) injectAutoSearchContext(gw *gateway.NenyaGateway, payload map[string]interface{}, messages []interface{}, serverName string, result *autoSearchResult, toolName, agentName string) {
	contextStr := fmt.Sprintf("[Memory context from %s]\n%s", serverName, result.text)
	memoryMsg := map[string]interface{}{
		"role":    "system",
		"content": contextStr,
	}

	updated := make([]interface{}, 0, util.AddCap(1, len(messages)))
	updated = append(updated, messages[:len(messages)-1]...)
	updated = append(updated, memoryMsg)
	updated = append(updated, messages[len(messages)-1:]...)
	payload["messages"] = updated

	gw.Logger.Debug("MCP auto-search context injected",
		"server", serverName, "agent", agentName,
		"tool", toolName,
		"duration_ms", result.duration.Milliseconds(),
		"result_len", len(result.text))
	gw.Metrics.RecordMCPAutoSearch(serverName, agentName, true, nil)
}

func (p *Proxy) forwardToUpstreamWithMCP(gw *gateway.NenyaGateway,
	w http.ResponseWriter,
	r *http.Request,
	opts forwardOptions) {
	_, hasAgent := gw.Config.Agents[opts.AgentName]
	maxIter := mcpMaxIterations
	if hasAgent {
		if agent := gw.Config.Agents[opts.AgentName]; agent.MCP != nil && agent.MCP.MaxIterations > 0 {
			maxIter = agent.MCP.MaxIterations
			if maxIter > mcpMaxIterationsHardCeiling {
				maxIter = mcpMaxIterationsHardCeiling
			}
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
		replayBufferedResponse(w, lastBuf, gw.Logger)
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

func (p *Proxy) mcpIteration(in mcpIterInput) int {
	select {
	case <-in.mcpLoopCtx.Done():
		in.gw.Logger.Warn("MCP loop deadline exceeded", "agent", in.opts.AgentName, "iterations", *in.actualIter)
		if *in.lastBuf != nil {
			replayBufferedResponse(in.w, *in.lastBuf, in.gw.Logger)
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

	buf, err := p.forwardBuffered(in.gw, in.mcpLoopCtx, in.r, in.opts.Targets, working, in.opts.Cooldown, in.opts.TokenCount, in.opts.AgentName, in.opts.MaxRetries)
	if err != nil {
		in.gw.Logger.Warn("MCP loop: upstream failed, streaming last response",
			"iteration", in.iteration, "err", err)
		if *in.lastBuf != nil {
			replayBufferedResponse(in.w, *in.lastBuf, in.gw.Logger)
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
		replayBufferedResponse(in.w, buf, in.gw.Logger)
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
		appendMCPResults(working, mcpCalls, results, mcpAssistantMsg)

		updatedPayload, err := json.Marshal(working)
		if err != nil {
			in.gw.Logger.Error("failed to marshal updated payload for MCP loop", "err", err)
			replayBufferedResponse(in.w, buf, in.gw.Logger)
			return mcpIterReturn
		}
		*in.originalPayload = updatedPayload
	}

	if len(mcpCalls) == 0 && len(nonMcpCalls) > 0 {
		in.gw.Logger.Debug("MCP loop: non-MCP tool calls only, replaying",
			"non_mcp_calls", len(nonMcpCalls),
			"raw_bytes_len", len(buf.rawBytes))
		replayBufferedResponse(in.w, buf, in.gw.Logger)
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

		action := p.prepareAndSend(gw, r, i, targets, target, workingPayload, cooldownDuration, tokenCount, agentName)
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
		buf, err := p.handleBufferedStream(ctx, action, target, gw, cooldownDuration)
		if err != nil {
			gw.AgentState.RecordFailure(target, cooldownDuration)
		}
		return buf, false
	}
	return nil, false
}

func (p *Proxy) handleBufferedStream(ctx context.Context, action upstreamAction, target routing.UpstreamTarget, gw *gateway.NenyaGateway, cooldownDuration time.Duration) (*bufferedSSE, error) {
	defer action.cancel()
	buf, err := bufferStreamResponse(ctx, action.resp.Body, gw.Logger)
	_ = action.resp.Body.Close()
	if err != nil {
		gw.AgentState.RecordFailure(target, cooldownDuration)
		return nil, fmt.Errorf("buffering response: %w", err)
	}
	gw.AgentState.RecordSuccess(target.CoolKey)
	return buf, nil
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

// applyRedactToContent runs redactFn against every text surface of msgNode's
// content, preserving multimodal content arrays instead of flattening them to
// a string. Returns true if any part was changed.
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

// applyRedactToContent applies the redact function to the content field of a message node.
// Supports both string content and content arrays (redacting only text parts).
// Returns true if any content was modified.
func applyRedactToContent(msgNode map[string]interface{}, redactFn func(string) string) bool {
	contentRaw, ok := msgNode["content"]
	if !ok {
		return false
	}
	changed := false
	switch c := contentRaw.(type) {
	case string:
		if c == "" {
			return false
		}
		if r := redactFn(c); r != c {
			msgNode["content"] = r
			changed = true
		}
	case []interface{}:
		for _, partRaw := range c {
			part, ok := partRaw.(map[string]interface{})
			if !ok {
				continue
			}
			if part["type"] != "text" {
				continue
			}
			text, ok := part["text"].(string)
			if !ok || text == "" {
				continue
			}
			if r := redactFn(text); r != text {
				part["text"] = r
				changed = true
			}
		}
	}
	return changed
}

// handleNonStreamingResponse buffers the full upstream response and returns it as a complete JSON object.
