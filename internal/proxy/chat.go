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
	"unicode/utf8"

	"nenya/internal/config"
	"nenya/internal/gateway"
	"nenya/internal/infra"
	"nenya/internal/mcp"
	"nenya/internal/pipeline"
	"nenya/internal/routing"
)

func (p *Proxy) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, p.Gateway().Config.Server.MaxBodyBytes)
	defer r.Body.Close()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		p.Gateway().Logger.Error("failed to read request body", "err", err)
		http.Error(w, "Payload too large or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	if r.Context().Err() != nil {
		return
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		p.Gateway().Logger.Warn("failed to parse JSON, returning Bad Request")
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	modelName, ok := payload["model"].(string)
	if !ok || modelName == "" {
		p.Gateway().Logger.Warn("missing or empty model field in request body")
		http.Error(w, `Missing or empty "model" field in request body`, http.StatusBadRequest)
		return
	}
	if len(modelName) > MaxModelNameLength {
		p.Gateway().Logger.Warn("model name exceeds maximum length", "length", len(modelName))
		http.Error(w, "Model name too long", http.StatusBadRequest)
		return
	}

	tokenCount := p.Gateway().CountRequestTokens(payload)

	var targets []routing.UpstreamTarget
	var cooldownDuration time.Duration
	var agentName string

	var maxRetries int
	if agent, ok := p.Gateway().Config.Agents[modelName]; ok {
		agentName = modelName
		secs := agent.CooldownSeconds
		if secs == 0 {
			secs = routing.DefaultAgentCooldownSec
		}
		cooldownDuration = time.Duration(secs) * time.Second
		maxRetries = agent.MaxRetries
		targets = p.Gateway().AgentState.BuildTargetList(p.Gateway().Logger, modelName, agent, tokenCount, p.Gateway().Providers)
		if len(targets) == 0 {
			if len(agent.Models) > 0 {
				p.Gateway().Logger.Warn("all models excluded by max_context",
					"agent", modelName, "tokens", tokenCount)
				http.Error(w, "Request too large for all configured models in this agent", http.StatusRequestEntityTooLarge)
			} else {
				p.Gateway().Logger.Error("agent has no models configured", "agent", modelName)
				http.Error(w, "Agent has no models configured", http.StatusInternalServerError)
			}
			return
		}
		strategy := agent.Strategy
		if strategy == "" {
			strategy = "round-robin"
		}
		p.Gateway().Logger.Info("agent routing",
			"agent", modelName, "strategy", strategy, "models_in_chain", len(targets))
	} else {
		provider := routing.ResolveProvider(modelName, p.Gateway().Providers)
		if provider == nil {
			p.Gateway().Logger.Warn("no provider found for model", "model", modelName)
			http.Error(w, "No provider configured for this model", http.StatusBadRequest)
			return
		}
		targets = []routing.UpstreamTarget{{URL: provider.URL, Model: modelName, Provider: provider.Name}}
		p.Gateway().Logger.Info("model routing", "model", modelName, "upstream", provider.URL)
	}

	var cacheKey string
	if p.Gateway().ResponseCache != nil {
		cacheKey = infra.FingerprintPayload(payload)
		if r.Header.Get(p.Gateway().Config.ResponseCache.ForceRefreshHeader) == "" {
			if data, ok := p.Gateway().ResponseCache.Lookup(cacheKey); ok {
				p.replayCachedSSE(w, r, data)
				return
			}
		}
	}

	if messagesRaw, ok := payload["messages"]; ok {
		if messages, ok := messagesRaw.([]interface{}); ok && len(messages) > 0 {
			p.injectAutoSearch(r.Context(), payload, messages, agentName)
			p.injectMCPTools(payload, agentName)
			windowMaxCtx := routing.ResolveWindowMaxContext(modelName, p.Gateway().Config.Agents)
			profile := pipeline.ClassifyClient(r.Header)
			if profile.IsIDE {
				p.Gateway().Logger.Debug("IDE client detected", "client", profile.ClientName)
			}
			if err := p.applyContentPipeline(r.Context(), payload, tokenCount, windowMaxCtx, profile); err != nil {
				p.Gateway().Logger.Warn("content pipeline failed, proceeding with original payload", "err", err)
			}
		} else {
			p.Gateway().Logger.Warn("messages field is not a non-empty array, skipping Ollama interception")
		}
	}

	if p.hasMCPTools(agentName) {
		p.forwardToUpstreamWithMCP(w, r, targets, payload, cooldownDuration, tokenCount, agentName, maxRetries, cacheKey)
		return
	}

	p.forwardToUpstream(w, r, targets, payload, cooldownDuration, tokenCount, agentName, maxRetries, cacheKey)
}

func (p *Proxy) replayCachedSSE(w http.ResponseWriter, r *http.Request, data []byte) {
	p.Gateway().Logger.Info("response cache hit")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Nenya-Cache-Status", "HIT")
	w.WriteHeader(http.StatusOK)

	var dst io.Writer
	if fw, ok := newImmediateFlushWriterSafe(w); ok {
		dst = fw
	} else {
		dst = w
	}
	buf := getStreamBuffer()
	defer streamingBufPool.Put(buf)
	if _, err := copyStream(r.Context(), dst, bytes.NewReader(data), *buf); err != nil {
		p.Gateway().Logger.Error("failed to replay cached SSE stream", "err", err)
	}
}

func (p *Proxy) applyContentPipeline(ctx context.Context, payload map[string]interface{}, tokenCount int, windowMaxCtx int, profile pipeline.ClientProfile) error {
	messages, ok := payload["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		return nil
	}

	pipeline.ApplyPrefixCacheOptimizations(payload, messages, p.Gateway().Config.PrefixCache)

	anyRedacted := false
	for _, msgRaw := range messages {
		msgNode, isMap := msgRaw.(map[string]interface{})
		if !isMap {
			continue
		}
		if pipeline.ShouldSkipRedaction(msgNode, p.Gateway().Config.PrefixCache) {
			continue
		}
		text := gateway.ExtractContentText(msgNode)
		if text == "" {
			continue
		}
		var redacted string
		if profile.IsIDE {
			redacted = pipeline.RedactSecretsPreservingCodeSpans(text, p.Gateway().Config.SecurityFilter.Enabled, p.Gateway().SecretPatterns, p.Gateway().Config.SecurityFilter.RedactionLabel)
		} else {
			redacted = pipeline.RedactSecrets(text, p.Gateway().Config.SecurityFilter.Enabled, p.Gateway().SecretPatterns, p.Gateway().Config.SecurityFilter.RedactionLabel)
		}
		if redacted != text {
			msgNode["content"] = redacted
			anyRedacted = true
		}
	}
	if anyRedacted {
		if p.Gateway().Metrics != nil {
			p.Gateway().Metrics.RecordRedaction()
		}
	}

	messages = payload["messages"].([]interface{})
	if len(messages) == 0 {
		return nil
	}
	if !profile.IsIDE {
		// Order matters: compaction normalizes whitespace first, which ensures
		// <think\r\n gets normalized to <think\n for thought pruning.
		if pipeline.ApplyCompaction(messages, p.Gateway().Config.Compaction) {
			if p.Gateway().Metrics != nil {
				p.Gateway().Metrics.RecordCompaction()
			}
		}
		if pipeline.PruneStaleToolCalls(payload, p.Gateway().Config.Compaction) {
			if p.Gateway().Metrics != nil {
				p.Gateway().Metrics.RecordCompaction()
			}
		}
		if pipeline.PruneThoughts(payload, p.Gateway().Config.Compaction) {
			if p.Gateway().Metrics != nil {
				p.Gateway().Metrics.RecordCompaction()
			}
		}
	} else {
		p.Gateway().Logger.Debug("skipping compaction for IDE client")
	}

	messages = payload["messages"].([]interface{})
	if len(messages) == 0 {
		return nil
	}
	deps := pipeline.WindowDeps{
		Logger:       p.Gateway().Logger,
		Client:       p.Gateway().Client,
		OllamaClient: p.Gateway().OllamaClient,
		Providers:    p.Gateway().Providers,
		InjectAPIKey: func(providerName string, headers http.Header) error {
			return routing.InjectAPIKey(providerName, p.Gateway().Providers, headers)
		},
		CountTokens: p.Gateway().CountTokens,
	}
	if windowed, err := pipeline.ApplyWindowCompaction(ctx, deps, payload, messages, tokenCount, p.Gateway().Config.Window, windowMaxCtx, p.Gateway().CountRequestTokens); err != nil {
		p.Gateway().Logger.Warn("window compaction failed, proceeding without it", "err", err)
	} else if windowed {
		if p.Gateway().Metrics != nil {
			p.Gateway().Metrics.RecordWindow(p.Gateway().Config.Window.Mode)
		}
	}

	messages = payload["messages"].([]interface{})
	if len(messages) == 0 {
		return nil
	}
	lastMsgRaw := messages[len(messages)-1]
	lastMsgNode, ok := lastMsgRaw.(map[string]interface{})
	if !ok {
		return nil
	}

	textForInterception := gateway.ExtractContentText(lastMsgNode)
	if textForInterception == "" {
		p.Gateway().Logger.Warn("last message has no text content, skipping interception")
		return nil
	}

	contentRunes := utf8.RuneCountInString(textForInterception)
	softLimit := p.Gateway().Config.Governance.ContextSoftLimit
	hardLimit := p.Gateway().Config.Governance.ContextHardLimit

	var processed string
	var needsUpdate bool
	var truncated string

	if contentRunes < softLimit {
		p.Gateway().Logger.Debug("payload within soft limit, passing through",
			"runes", contentRunes, "soft_limit", softLimit)
	} else if contentRunes <= hardLimit {
		p.Gateway().Logger.Warn("payload exceeds soft limit, sending to engine",
			"runes", contentRunes)
		if p.Gateway().Metrics != nil {
			p.Gateway().Metrics.RecordInterception("soft_limit")
		}
		summarized, err := p.summarizeWithOllama(ctx, textForInterception, profile.IsIDE)
		if err != nil {
			p.Gateway().Logger.Warn("engine summarization failed, proceeding with original payload", "err", err)
		} else {
			processed = fmt.Sprintf("[Nenya Sanitized via Ollama]:\n%s", summarized)
			needsUpdate = true
		}
	} else {
		p.Gateway().Logger.Warn("payload exceeds hard limit, truncating before engine",
			"runes", contentRunes, "hard_limit", hardLimit)
		if p.Gateway().Metrics != nil {
			p.Gateway().Metrics.RecordInterception("hard_limit")
		}

		querySource := p.Gateway().Config.Governance.TFIDFQuerySource
		if querySource != "" {
			var query string
			switch querySource {
			case "prior_messages":
				query = pipeline.ExtractPriorUserMessages(messages[:len(messages)-1], 5)
			case "self":
				query = pipeline.ExtractSelfQuery(textForInterception, 500)
			}
			p.Gateway().Logger.Info("TF-IDF truncation enabled",
				"query_source", querySource,
				"query_len", utf8.RuneCountInString(query),
				"input_runes", contentRunes)

			if profile.IsIDE {
				truncated = pipeline.TruncateTFIDFCodeAware(textForInterception, hardLimit, query, p.Gateway().Config.Governance)
			} else {
				truncated = pipeline.TruncateTFIDF(textForInterception, hardLimit, query, p.Gateway().Config.Governance)
			}

			if utf8.RuneCountInString(truncated) < softLimit {
				p.Gateway().Logger.Info("TF-IDF reduced payload below soft limit, skipping engine",
					"truncated_runes", utf8.RuneCountInString(truncated), "soft_limit", softLimit)
				processed = fmt.Sprintf("[Nenya TF-IDF Pruned]:\n%s", truncated)
				needsUpdate = true
			} else {
				processed, needsUpdate = p.summarizeOrForward(ctx, truncated, profile.IsIDE, "TF-IDF Pruned")
			}
		} else {
			if profile.IsIDE {
				truncated = pipeline.TruncateMiddleOutCodeAware(textForInterception, hardLimit, p.Gateway().Config.Governance)
			} else {
				truncated = pipeline.TruncateMiddleOut(textForInterception, hardLimit, p.Gateway().Config.Governance)
			}
			processed, needsUpdate = p.summarizeOrForward(ctx, truncated, profile.IsIDE, "Truncated")
		}
	}

	if needsUpdate {
		lastMsgNode["content"] = processed
	}

	return nil
}

func (p *Proxy) summarizeWithOllama(ctx context.Context, heavyText string, isIDE bool) (string, error) {
	if len(p.Gateway().Config.SecurityFilter.Engine.ResolvedTargets) == 0 {
		return "", fmt.Errorf("security_filter engine: no resolved targets")
	}

	defaultPrompt := "You are a data privacy filter. Review the following text and remove or replace any IP addresses, AWS keys (AKIA...), passwords, tokens, or credentials with [REDACTED]. Preserve the original structure, detail level, and all non-sensitive content exactly as provided. Do NOT summarize or shorten the text."

	if isIDE && pipeline.HasCodeFences(heavyText) {
		defaultPrompt = "You are a data privacy filter for code. The following text contains code blocks (marked with ``` fences). Remove or replace any IP addresses, AWS keys (AKIA...), passwords, tokens, or credentials that appear OUTSIDE code blocks with [REDACTED]. Inside code blocks, only redact actual hardcoded secrets in string literals — preserve all code structure, function signatures, import statements, variable names, and line-number references exactly. Do NOT summarize, shorten, or restructure any code. Do NOT modify non-sensitive code."
	}

	ref := p.Gateway().Config.SecurityFilter.Engine
	systemPrompt, err := config.LoadPromptFile(ref.SystemPromptFile, ref.SystemPrompt, defaultPrompt)
	if err != nil {
		p.Gateway().Logger.Warn("failed to load privacy filter prompt, using default", "err", err)
		systemPrompt = defaultPrompt
	}

	agentName := ref.AgentName
	if agentName == "" {
		agentName = "inline"
	}

	return pipeline.CallEngineChain(ctx, p.Gateway().Client, p.Gateway().OllamaClient,
		ref.ResolvedTargets, p.Gateway().Logger,
		func(providerName string, headers http.Header) error {
			return routing.InjectAPIKey(providerName, p.Gateway().Providers, headers)
		},
		"security_filter", agentName, systemPrompt, heavyText)
}

func (p *Proxy) summarizeOrForward(ctx context.Context, truncated string, isIDE bool, label string) (string, bool) {
	summarized, err := p.summarizeWithOllama(ctx, truncated, isIDE)
	if err != nil {
		if p.Gateway().Config.SecurityFilter.SkipOnEngineFailure {
			p.Gateway().Logger.Warn("engine summarization failed, skip_on_engine_failure=true, forwarding original payload", "err", err)
			return "", false
		}
		p.Gateway().Logger.Warn("engine summarization failed after truncation, forwarding truncated", "err", err)
		return fmt.Sprintf("[Nenya %s (engine unreachable)]:\n%s", label, truncated), true
	}
	return fmt.Sprintf("[Nenya Sanitized via Ollama (%s input)]:\n%s", label, summarized), true
}

func (p *Proxy) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, p.Gateway().Config.Server.MaxBodyBytes)
	defer r.Body.Close()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		p.Gateway().Logger.Error("failed to read embeddings request body", "err", err)
		http.Error(w, "Payload too large or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	var payload map[string]interface{}
	if err = json.Unmarshal(bodyBytes, &payload); err != nil {
		p.Gateway().Logger.Warn("failed to parse embeddings JSON")
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	modelName, ok := payload["model"].(string)
	if !ok || modelName == "" {
		p.Gateway().Logger.Warn("missing or empty model in embeddings request")
		http.Error(w, `Missing or empty "model" field`, http.StatusBadRequest)
		return
	}
	if len(modelName) > MaxModelNameLength {
		p.Gateway().Logger.Warn("model name exceeds maximum length in embeddings request", "length", len(modelName))
		http.Error(w, "Model name too long", http.StatusBadRequest)
		return
	}

	provider := routing.ResolveProvider(modelName, p.Gateway().Providers)
	if provider == nil {
		p.Gateway().Logger.Warn("no provider for embeddings model", "model", modelName)
		http.Error(w, "No provider configured for this model", http.StatusBadRequest)
		return
	}

	embeddingURL := strings.TrimSuffix(provider.URL, "/chat/completions") + "/embeddings"
	if embeddingURL == provider.URL {
		p.Gateway().Logger.Warn("provider URL does not end with /chat/completions, cannot derive embeddings endpoint",
			"provider", provider.Name, "url", provider.URL)
		http.Error(w, "Provider does not support embeddings", http.StatusBadRequest)
		return
	}

	req, err := p.buildUpstreamRequest(r.Context(), http.MethodPost, embeddingURL, bodyBytes, provider.Name, r.Header)
	if err != nil {
		p.Gateway().Logger.Error("failed to create embeddings upstream request", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	ctx := r.Context()
	if provider.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(r.Context(), time.Duration(provider.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	resp, err := p.Gateway().Client.Do(req.WithContext(ctx))
	if err != nil {
		p.Gateway().Logger.Error("embeddings upstream request failed", "provider", provider.Name, "err", err)
		http.Error(w, "Upstream provider error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	routing.CopyHeaders(resp.Header, w.Header())
	w.WriteHeader(resp.StatusCode)

	if _, err := io.Copy(w, resp.Body); err != nil {
		p.Gateway().Logger.Debug("embeddings response copy ended", "err", err)
	}
}

func (p *Proxy) handleResponses(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, p.Gateway().Config.Server.MaxBodyBytes)
	defer r.Body.Close()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		p.Gateway().Logger.Error("failed to read responses request body", "err", err)
		http.Error(w, "Payload too large or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	var payload map[string]interface{}
	err = json.Unmarshal(bodyBytes, &payload)
	if err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	modelName, ok := payload["model"].(string)
	if !ok || modelName == "" {
		http.Error(w, `Missing or empty "model" field`, http.StatusBadRequest)
		return
	}

	provider := routing.ResolveProvider(modelName, p.Gateway().Providers)
	if provider == nil {
		p.Gateway().Logger.Warn("no provider for responses model", "model", modelName)
		http.Error(w, "No provider configured for this model", http.StatusBadRequest)
		return
	}

	responsesURL := strings.TrimSuffix(provider.URL, "/chat/completions") + "/responses"
	if responsesURL == provider.URL {
		p.Gateway().Logger.Warn("provider URL does not end with /chat/completions, cannot derive responses endpoint",
			"provider", provider.Name, "url", provider.URL)
		http.Error(w, "Provider does not support responses API", http.StatusBadRequest)
		return
	}

	req, err := p.buildUpstreamRequest(r.Context(), http.MethodPost, responsesURL, bodyBytes, provider.Name, r.Header)
	if err != nil {
		p.Gateway().Logger.Error("failed to create responses upstream request", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	ctx := r.Context()
	if provider.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(r.Context(), time.Duration(provider.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	resp, err := p.Gateway().Client.Do(req.WithContext(ctx))
	if err != nil {
		p.Gateway().Logger.Error("responses upstream request failed", "provider", provider.Name, "err", err)
		http.Error(w, "Upstream provider error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	routing.CopyHeaders(resp.Header, w.Header())
	w.WriteHeader(resp.StatusCode)

	if _, err := io.Copy(w, resp.Body); err != nil {
		p.Gateway().Logger.Debug("responses response copy ended", "err", err)
	}
}

func (p *Proxy) buildUpstreamRequest(ctx context.Context, method, url string, body []byte, providerName string, srcHeaders http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create upstream request: %w", err)
	}
	headers := srcHeaders.Clone()
	headers.Del("Authorization")
	if err := routing.InjectAPIKey(providerName, p.Gateway().Providers, headers); err != nil {
		return nil, fmt.Errorf("API key injection failed: %w", err)
	}
	routing.CopyHeaders(headers, req.Header)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", p.Gateway().Config.Server.UserAgent)
	return req, nil
}

func (p *Proxy) hasMCPTools(agentName string) bool {
	if agentName == "" {
		return false
	}
	agent, ok := p.Gateway().Config.Agents[agentName]
	if !ok || agent.MCP == nil || len(agent.MCP.Servers) == 0 {
		return false
	}
	for _, serverName := range agent.MCP.Servers {
		if client, ok := p.Gateway().MCPClients[serverName]; ok && client.Ready() {
			return true
		}
	}
	return false
}

func (p *Proxy) injectMCPTools(payload map[string]interface{}, agentName string) {
	if agentName == "" {
		return
	}
	agent, ok := p.Gateway().Config.Agents[agentName]
	if !ok || agent.MCP == nil || len(agent.MCP.Servers) == 0 {
		return
	}

	p.Gateway().Logger.Info("MCP injection starting",
		"servers", agent.MCP.Servers, "agent", agentName)

	var toolNames []string
	for _, serverName := range agent.MCP.Servers {
		client, ok := p.Gateway().MCPClients[serverName]
		if !ok || !client.Ready() {
			p.Gateway().Logger.Warn("MCP server not available, skipping tool injection",
				"server", serverName, "agent", agentName)
			continue
		}

		tools := client.ListTools()
		if len(tools) == 0 {
			p.Gateway().Logger.Warn("MCP server returned no tools",
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
		p.Gateway().Logger.Debug("MCP tools injected",
			"server", serverName, "tools", len(tools), "agent", agentName)
	}

	if len(toolNames) > 0 {
		if _, has := payload["tool_choice"]; !has {
			payload["tool_choice"] = "auto"
			p.Gateway().Logger.Info("MCP tool_choice auto injected",
				"tools_count", len(toolNames), "agent", agentName)
		}
		p.injectMCPSystemPrompt(payload, toolNames)
	} else {
		p.Gateway().Logger.Warn("MCP: no tools injected for agent",
			"agent", agentName, "servers", agent.MCP.Servers)
	}
}

func (p *Proxy) injectMCPSystemPrompt(payload map[string]interface{}, toolNames []string) {
	var toolsList strings.Builder
	for i, name := range toolNames {
		if i > 0 {
			toolsList.WriteString(", ")
		}
		toolsList.WriteString("`" + name + "`")
	}

	prompt := fmt.Sprintf(
		"You have access to the following MCP tools for long-term memory and knowledge retrieval: %s. "+
			"Use these tools when the user asks about previously discussed information, needs to recall past "+
			"conversations, or explicitly requests memory/knowledge operations. Do NOT mention these tools "+
			"unless the user's query requires accessing stored information.",
		toolsList.String(),
	)

	messages, ok := payload["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		return
	}

	mcpMsg := map[string]interface{}{
		"role":    "system",
		"content": prompt,
	}

	updated := make([]interface{}, 0, len(messages)+1)
	updated = append(updated, mcpMsg)
	updated = append(updated, messages...)
	payload["messages"] = updated

	p.Gateway().Logger.Debug("MCP system prompt injected", "tools", len(toolNames))
}

func (p *Proxy) discoverToolByPrefix(serverName, prefix string) string {
	client, ok := p.Gateway().MCPClients[serverName]
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

func (p *Proxy) injectAutoSearch(ctx context.Context, payload map[string]interface{}, messages []interface{}, agentName string) {
	if agentName == "" {
		return
	}
	agent, ok := p.Gateway().Config.Agents[agentName]
	if !ok || agent.MCP == nil || !agent.MCP.AutoSearch {
		return
	}

	if len(messages) == 0 {
		return
	}
	lastMsg, ok := messages[len(messages)-1].(map[string]interface{})
	if !ok {
		return
	}
	lastRole, _ := lastMsg["role"].(string)
	if lastRole != "user" {
		return
	}

	query := gateway.ExtractContentText(lastMsg)
	if query == "" {
		return
	}

	searchTool := agent.MCP.SearchTool
	for _, serverName := range agent.MCP.Servers {
		client, ok := p.Gateway().MCPClients[serverName]
		if !ok || !client.Ready() {
			continue
		}

		toolName := searchTool
		if toolName == "" {
			toolName = p.discoverToolByPrefix(serverName, "search")
			if toolName == "" {
				p.Gateway().Logger.Warn("MCP auto-search: no 'search' tool found on server",
					"server", serverName, "agent", agentName)
				continue
			}
		}

		start := time.Now()
		result, err := client.CallTool(ctx, toolName, map[string]any{
			"query": query,
			"limit": 5,
		})
		duration := time.Since(start)
		if err != nil {
			p.Gateway().Logger.Warn("MCP auto-search failed, proceeding without",
				"server", serverName, "agent", agentName, "err", err,
				"duration_ms", duration.Milliseconds())
			if p.Gateway().Metrics != nil {
				p.Gateway().Metrics.RecordMCPAutoSearch(serverName, agentName, false, err)
			}
			continue
		}
		if result == nil || result.Text() == "" {
			p.Gateway().Logger.Debug("MCP auto-search: no results",
				"server", serverName, "agent", agentName,
				"duration_ms", duration.Milliseconds())
			if p.Gateway().Metrics != nil {
				p.Gateway().Metrics.RecordMCPAutoSearch(serverName, agentName, false, nil)
			}
			continue
		}

		contextStr := fmt.Sprintf("[Memory context from %s]\n%s", serverName, result.Text())
		memoryMsg := map[string]interface{}{
			"role":    "system",
			"content": contextStr,
		}

		updated := make([]interface{}, 0, len(messages)+1)
		updated = append(updated, messages[:len(messages)-1]...)
		updated = append(updated, memoryMsg)
		updated = append(updated, messages[len(messages)-1:]...)
		payload["messages"] = updated

		p.Gateway().Logger.Debug("MCP auto-search context injected",
			"server", serverName, "agent", agentName,
			"tool", toolName,
			"duration_ms", duration.Milliseconds(),
			"result_len", len(result.Text()))
		if p.Gateway().Metrics != nil {
			p.Gateway().Metrics.RecordMCPAutoSearch(serverName, agentName, true, nil)
		}
		break
	}
}

func (p *Proxy) forwardToUpstreamWithMCP(
	w http.ResponseWriter,
	r *http.Request,
	targets []routing.UpstreamTarget,
	payload map[string]interface{},
	cooldownDuration time.Duration,
	tokenCount int,
	agentName string,
	maxRetries int,
	cacheKey string,
) {
	_, hasAgent := p.Gateway().Config.Agents[agentName]
	maxIter := mcpMaxIterations
	if hasAgent {
		if agent := p.Gateway().Config.Agents[agentName]; agent.MCP != nil && agent.MCP.MaxIterations > 0 {
			maxIter = agent.MCP.MaxIterations
		}
	}

	originalPayload, err := json.Marshal(payload)
	if err != nil {
		p.Gateway().Logger.Error("failed to marshal payload for MCP loop", "err", err)
		writeSSEError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	var lastBuf *bufferedSSE
	loopStart := time.Now()
	totalToolCalls := 0
	actualIter := 0

	defer func() {
		loopDuration := time.Since(loopStart)
		if p.Gateway().Metrics != nil && loopDuration > 0 {
			p.Gateway().Metrics.RecordMCPLoopDuration(agentName, loopDuration)
		}
		p.Gateway().Logger.Info("MCP multi-turn loop completed",
			"agent", agentName,
			"iterations", actualIter,
			"tool_calls_executed", totalToolCalls,
			"duration_ms", loopDuration.Milliseconds())
	}()

	for iteration := 0; iteration < maxIter; iteration++ {
		if p.Gateway().Metrics != nil {
			p.Gateway().Metrics.RecordMCPLoopIteration(agentName)
		}
		actualIter++

		working := make(map[string]interface{})
		if err := json.Unmarshal(originalPayload, &working); err != nil {
			p.Gateway().Logger.Error("failed to unmarshal payload for MCP iteration", "err", err)
			break
		}

		if iteration > 0 {
			payload = working
		} else {
			// Use the already-parsed payload for first iteration
			working = payload
		}

		buf, err := p.forwardBuffered(r, targets, working, cooldownDuration, tokenCount, agentName, maxRetries)
		if err != nil {
			p.Gateway().Logger.Warn("MCP loop: upstream failed, streaming last response",
				"iteration", iteration, "err", err)
			if lastBuf != nil {
				replayBufferedResponse(w, lastBuf, p.Gateway().Logger)
				return
			}
			writeSSEError(w, http.StatusBadGateway, "All upstream providers failed")
			return
		}

		allCalls := buf.toolCalls
		if len(allCalls) == 0 {
			p.Gateway().Logger.Debug("MCP loop: content-only response, replaying",
				"has_content", buf.hasContent,
				"finish_reason", buf.finishReason,
				"raw_bytes_len", len(buf.rawBytes))
			replayBufferedResponse(w, buf, p.Gateway().Logger)
			p.recordMCPUsage(buf, agentName)
			return
		}

		mcpCalls, nonMcpCalls := partitionMCPToolCalls(allCalls, p.Gateway().MCPToolIndex)
		totalToolCalls += len(mcpCalls)

		if len(mcpCalls) > 0 {
			p.Gateway().Logger.Info("MCP tool calls intercepted",
				"mcp_calls", len(mcpCalls),
				"non_mcp_calls", len(nonMcpCalls),
				"iteration", iteration+1,
				"agent", agentName)

			results := executeMCPCalls(r.Context(), mcpCalls, p)
			appendMCPResults(working, mcpCalls, results, buf.assistantMessage)

			updatedPayload, err := json.Marshal(working)
			if err != nil {
				p.Gateway().Logger.Error("failed to marshal updated payload for MCP loop", "err", err)
				replayBufferedResponse(w, buf, p.Gateway().Logger)
				return
			}
			originalPayload = updatedPayload
		}

		if len(mcpCalls) == 0 && len(nonMcpCalls) > 0 {
			p.Gateway().Logger.Debug("MCP loop: non-MCP tool calls only, replaying",
				"non_mcp_calls", len(nonMcpCalls),
				"raw_bytes_len", len(buf.rawBytes))
			replayBufferedResponse(w, buf, p.Gateway().Logger)
			p.recordMCPUsage(buf, agentName)
			return
		}

		lastBuf = buf
	}

	if lastBuf != nil {
		p.Gateway().Logger.Warn("MCP loop exhausted, replaying last response",
			"max_iterations", maxIter, "agent", agentName)
		replayBufferedResponse(w, lastBuf, p.Gateway().Logger)
		p.recordMCPUsage(lastBuf, agentName)
		return
	}

	http.Error(w, "MCP loop ended without response", http.StatusInternalServerError)
}

func (p *Proxy) forwardBuffered(
	r *http.Request,
	targets []routing.UpstreamTarget,
	payload map[string]interface{},
	cooldownDuration time.Duration,
	tokenCount int,
	agentName string,
	maxRetries int,
) (*bufferedSSE, error) {
	originalPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	if p.Gateway().Config.Compaction.Enabled && p.Gateway().Config.Compaction.JSONMinify {
		minified := bytes.NewBuffer(make([]byte, 0, len(originalPayload)))
		if err := json.Compact(minified, originalPayload); err == nil {
			originalPayload = minified.Bytes()
		}
	}

	attempt := 0
	for i, target := range targets {
		if maxRetries > 0 && attempt >= maxRetries {
			p.Gateway().Logger.Warn("max retries reached in buffered mode",
				"attempt", attempt, "max", maxRetries, "agent", agentName)
			break
		}

		workingPayload := make(map[string]interface{})
		if err := json.Unmarshal(originalPayload, &workingPayload); err != nil {
			p.Gateway().Logger.Error("failed to unmarshal payload for target",
				"target", i+1, "total", len(targets), "err", err)
			continue
		}

		action := p.prepareAndSend(r, i, targets, target, workingPayload, cooldownDuration, tokenCount, agentName)
		switch action.kind {
		case actionContinue:
			continue
		case actionError:
			attempt++
			action.body, _ = io.ReadAll(io.LimitReader(action.resp.Body, pipeline.MaxErrorBodyBytes))
			action.resp.Body.Close()
			p.Gateway().Logger.Debug("MCP buffered: upstream error",
				"target", i+1,
				"status", action.resp.StatusCode,
				"model", target.Model,
				"body_len", len(action.body))
			shouldRetry, retryDelay := p.handleUpstreamError(i, targets, target, cooldownDuration, agentName, action)
			action.cancel()
			if shouldRetry {
				if maxRetries > 0 && attempt >= maxRetries {
					p.Gateway().Logger.Warn("max retries reached in buffered mode after error",
						"attempt", attempt, "max", maxRetries, "agent", agentName)
					break
				}
				if retryDelay > 0 {
					p.Gateway().Logger.Info("retrying with parsed delay (buffered)",
						"model", target.Model, "delay_ms", retryDelay.Milliseconds())
					waitWithCancel(r.Context(), retryDelay)
				} else {
					backoff := calculateBackoff(attempt - 1)
					p.Gateway().Logger.Info("retrying with exponential backoff (buffered)",
						"model", target.Model, "attempt", attempt, "delay_ms", backoff.Milliseconds())
					waitWithCancel(r.Context(), backoff)
				}
				continue
			}
			return nil, fmt.Errorf("upstream error: status %d", action.resp.StatusCode)
		case actionStream:
			defer action.cancel()
			buf, err := bufferStreamResponse(r.Context(), action.resp.Body)
			action.resp.Body.Close()
			if err != nil {
				p.Gateway().AgentState.RecordSuccess(target.CoolKey)
				return nil, fmt.Errorf("buffering response: %w", err)
			}
			p.Gateway().AgentState.RecordSuccess(target.CoolKey)
			return buf, nil
		}
	}

	p.Gateway().Logger.Error("all upstream targets exhausted (buffered)",
		"total", len(targets), "attempts", attempt)
	return nil, fmt.Errorf("all %d upstream targets exhausted", len(targets))
}

func (p *Proxy) recordMCPUsage(buf *bufferedSSE, agentName string) {
	// Usage is tracked by the upstream forwarding, but for MCP loop
	// iterations, we should record the token usage from the final response
	// via the existing stream metrics if available
	_ = buf
	_ = agentName
}
