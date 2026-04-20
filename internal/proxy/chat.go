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

func (p *Proxy) handleChatCompletions(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, gw.Config.Server.MaxBodyBytes)
	defer r.Body.Close()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		gw.Logger.Error("failed to read request body", "err", err)
		http.Error(w, "Payload too large or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	if r.Context().Err() != nil {
		return
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		gw.Logger.Warn("failed to parse JSON, returning Bad Request")
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	modelName, ok := payload["model"].(string)
	if !ok || modelName == "" {
		gw.Logger.Warn("missing or empty model field in request body")
		http.Error(w, `Missing or empty "model" field in request body`, http.StatusBadRequest)
		return
	}
	if len(modelName) > MaxModelNameLength {
		gw.Logger.Warn("model name exceeds maximum length", "length", len(modelName))
		http.Error(w, "Model name too long", http.StatusBadRequest)
		return
	}

	tokenCount := gw.CountRequestTokens(payload)

	var targets []routing.UpstreamTarget
	var cooldownDuration time.Duration
	var agentName string

	var maxRetries int
	if agent, ok := gw.Config.Agents[modelName]; ok {
		agentName = modelName
		secs := agent.CooldownSeconds
		if secs == 0 {
			secs = routing.DefaultAgentCooldownSec
		}
		cooldownDuration = time.Duration(secs) * time.Second
		maxRetries = agent.MaxRetries
		targets = gw.AgentState.BuildTargetList(gw.Logger, modelName, agent, tokenCount, gw.Providers)
		if len(targets) == 0 {
			if len(agent.Models) > 0 {
				gw.Logger.Warn("all models excluded by max_context",
					"agent", modelName, "tokens", tokenCount)
				http.Error(w, "Request too large for all configured models in this agent", http.StatusRequestEntityTooLarge)
			} else {
				gw.Logger.Error("agent has no models configured", "agent", modelName)
				http.Error(w, "Agent has no models configured", http.StatusInternalServerError)
			}
			return
		}
		strategy := agent.Strategy
		if strategy == "" {
			strategy = "round-robin"
		}
		gw.Logger.Info("agent routing",
			"agent", modelName, "strategy", strategy, "models_in_chain", len(targets))
	} else {
		provider := routing.ResolveProvider(modelName, gw.Providers)
		if provider == nil {
			gw.Logger.Warn("no provider found for model", "model", modelName)
			http.Error(w, "No provider configured for this model", http.StatusBadRequest)
			return
		}
		targets = []routing.UpstreamTarget{{URL: provider.URL, Model: modelName, Provider: provider.Name}}
		gw.Logger.Info("model routing", "model", modelName, "upstream", provider.URL)
	}

	var cacheKey string
	if gw.ResponseCache != nil {
		cacheKey = infra.FingerprintPayload(payload)
		if r.Header.Get(gw.Config.ResponseCache.ForceRefreshHeader) == "" {
			if data, ok := gw.ResponseCache.Lookup(cacheKey); ok {
				p.replayCachedSSE(gw, w, r, data)
				return
			}
		}
	}

	if messagesRaw, ok := payload["messages"]; ok {
		if messages, ok := messagesRaw.([]interface{}); ok && len(messages) > 0 {
			p.injectAutoSearch(gw, r.Context(), payload, messages, agentName)
			p.injectMCPTools(gw, payload, agentName)
			windowMaxCtx := routing.ResolveWindowMaxContext(modelName, gw.Config.Agents)
			profile := pipeline.ClassifyClient(r.Header)
			if profile.IsIDE {
				gw.Logger.Debug("IDE client detected", "client", profile.ClientName)
			}

			// Derive soft/hard limits from the primary target's MaxContext
			softLimit := 4000
			hardLimit := 24000
			if len(targets) > 0 {
				primaryTarget := targets[0]
				if primaryTarget.MaxContext > 0 {
					tokenRatio := gw.Config.Server.TokenRatio
					if tokenRatio == 0 {
						tokenRatio = 4.0
					}
					softLimit = int(float64(primaryTarget.MaxContext) / tokenRatio * 0.125)
					hardLimit = int(float64(primaryTarget.MaxContext) / tokenRatio * 0.75)
				}
			}

			if err := p.applyContentPipeline(gw, r.Context(), payload, tokenCount, windowMaxCtx, profile, softLimit, hardLimit); err != nil {
				gw.Logger.Warn("content pipeline failed, proceeding with original payload", "err", err)
			}
		} else {
			gw.Logger.Warn("messages field is not a non-empty array, skipping Ollama interception")
		}
	}

	if p.hasMCPTools(gw, agentName) {
		p.forwardToUpstreamWithMCP(gw, w, r, targets, payload, cooldownDuration, tokenCount, agentName, maxRetries, cacheKey)
		return
	}

	p.forwardToUpstream(gw, w, r, targets, payload, cooldownDuration, tokenCount, agentName, maxRetries, cacheKey)
}

func (p *Proxy) replayCachedSSE(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, data []byte) {
	gw.Logger.Info("response cache hit")
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
		gw.Logger.Error("failed to replay cached SSE stream", "err", err)
	}
}

func (p *Proxy) applyContentPipeline(gw *gateway.NenyaGateway, ctx context.Context, payload map[string]interface{}, tokenCount int, windowMaxCtx int, profile pipeline.ClientProfile, softLimit, hardLimit int) error {
	messages, ok := payload["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		return nil
	}

	pipeline.ApplyPrefixCacheOptimizations(payload, messages, gw.Config.PrefixCache)

	anyRedacted := false
	for _, msgRaw := range messages {
		msgNode, isMap := msgRaw.(map[string]interface{})
		if !isMap {
			continue
		}
		if pipeline.ShouldSkipRedaction(msgNode, gw.Config.PrefixCache) {
			continue
		}
		text := gateway.ExtractContentText(msgNode)
		if text == "" {
			continue
		}
		var redacted string
		if profile.IsIDE {
			redacted = pipeline.RedactSecretsPreservingCodeSpans(text, gw.Config.SecurityFilter.Enabled, gw.SecretPatterns, gw.Config.SecurityFilter.RedactionLabel)
		} else {
			redacted = pipeline.RedactSecrets(text, gw.Config.SecurityFilter.Enabled, gw.SecretPatterns, gw.Config.SecurityFilter.RedactionLabel)
		}
		if redacted != text {
			msgNode["content"] = redacted
			anyRedacted = true
		}
	}
	if anyRedacted {
		if gw.Metrics != nil {
			gw.Metrics.RecordRedaction()
		}
	}

	if gw.EntropyFilter != nil {
		for _, msgRaw := range messages {
			msgNode, isMap := msgRaw.(map[string]interface{})
			if !isMap {
				continue
			}
			if pipeline.ShouldSkipRedaction(msgNode, gw.Config.PrefixCache) {
				continue
			}
			text := gateway.ExtractContentText(msgNode)
			if text == "" {
				continue
			}

			var redacted string
			if profile.IsIDE {
				redacted = gw.EntropyFilter.RedactHighEntropyPreservingCodeSpans(
					text, pipeline.DetectCodeFences(text), gw.Config.SecurityFilter.RedactionLabel,
				)
			} else {
				redacted = gw.EntropyFilter.RedactHighEntropy(text, gw.Config.SecurityFilter.RedactionLabel)
			}
			if redacted != text {
				msgNode["content"] = redacted
				anyRedacted = true
			}
		}
		if anyRedacted {
			if gw.Metrics != nil {
				gw.Metrics.RecordRedaction()
			}
		}
	}

	messages = payload["messages"].([]interface{})
	if len(messages) == 0 {
		return nil
	}
	if !profile.IsIDE {
		// Order matters: compaction normalizes whitespace first, which ensures
		// <think\r\n gets normalized to <think\n for thought pruning.
		if pipeline.ApplyCompaction(messages, gw.Config.Compaction) {
			if gw.Metrics != nil {
				gw.Metrics.RecordCompaction()
			}
		}
		if pipeline.PruneStaleToolCalls(payload, gw.Config.Compaction) {
			if gw.Metrics != nil {
				gw.Metrics.RecordCompaction()
			}
		}
		if pipeline.PruneThoughts(payload, gw.Config.Compaction) {
			if gw.Metrics != nil {
				gw.Metrics.RecordCompaction()
			}
		}
	} else {
		gw.Logger.Debug("skipping compaction for IDE client")
	}

	messages = payload["messages"].([]interface{})
	if len(messages) == 0 {
		return nil
	}
	deps := pipeline.WindowDeps{
		Logger:       gw.Logger,
		Client:       gw.Client,
		OllamaClient: gw.OllamaClient,
		Providers:    gw.Providers,
		InjectAPIKey: func(providerName string, headers http.Header) error {
			return routing.InjectAPIKey(providerName, gw.Providers, headers)
		},
		CountTokens: gw.CountTokens,
	}
	if windowed, err := pipeline.ApplyWindowCompaction(ctx, deps, payload, messages, tokenCount, gw.Config.Window, windowMaxCtx, gw.CountRequestTokens); err != nil {
		gw.Logger.Warn("window compaction failed, proceeding without it", "err", err)
	} else if windowed {
		if gw.Metrics != nil {
			gw.Metrics.RecordWindow(gw.Config.Window.Mode)
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
		gw.Logger.Warn("last message has no text content, skipping interception")
		return nil
	}

	contentRunes := utf8.RuneCountInString(textForInterception)

	var processed string
	var needsUpdate bool
	var truncated string

	if contentRunes < softLimit {
		gw.Logger.Debug("payload within soft limit, passing through",
			"runes", contentRunes, "soft_limit", softLimit)
	} else if contentRunes <= hardLimit {
		gw.Logger.Warn("payload exceeds soft limit, sending to engine",
			"runes", contentRunes)
		if gw.Metrics != nil {
			gw.Metrics.RecordInterception("soft_limit")
		}
		summarized, err := p.summarizeWithOllama(gw, ctx, textForInterception, profile.IsIDE)
		if err != nil {
			gw.Logger.Warn("engine summarization failed, proceeding with original payload", "err", err)
		} else {
			processed = fmt.Sprintf("[Nenya Sanitized via Ollama]:\n%s", summarized)
			needsUpdate = true
		}
	} else {
		gw.Logger.Warn("payload exceeds hard limit, truncating before engine",
			"runes", contentRunes, "hard_limit", hardLimit)
		if gw.Metrics != nil {
			gw.Metrics.RecordInterception("hard_limit")
		}

		querySource := gw.Config.Governance.TFIDFQuerySource
		if querySource != "" {
			var query string
			switch querySource {
			case "prior_messages":
				query = pipeline.ExtractPriorUserMessages(messages[:len(messages)-1], 5)
			case "self":
				query = pipeline.ExtractSelfQuery(textForInterception, 500)
			}
			gw.Logger.Info("TF-IDF truncation enabled",
				"query_source", querySource,
				"query_len", utf8.RuneCountInString(query),
				"input_runes", contentRunes)

			if profile.IsIDE {
				truncated = pipeline.TruncateTFIDFCodeAware(textForInterception, hardLimit, query, gw.Config.Governance)
			} else {
				truncated = pipeline.TruncateTFIDF(textForInterception, hardLimit, query, gw.Config.Governance)
			}

			if utf8.RuneCountInString(truncated) < softLimit {
				gw.Logger.Info("TF-IDF reduced payload below soft limit, skipping engine",
					"truncated_runes", utf8.RuneCountInString(truncated), "soft_limit", softLimit)
				processed = fmt.Sprintf("[Nenya TF-IDF Pruned]:\n%s", truncated)
				needsUpdate = true
			} else {
				processed, needsUpdate = p.summarizeOrForward(gw, ctx, truncated, profile.IsIDE, "TF-IDF Pruned")
			}
		} else {
			if profile.IsIDE {
				truncated = pipeline.TruncateMiddleOutCodeAware(textForInterception, hardLimit, gw.Config.Governance)
			} else {
				truncated = pipeline.TruncateMiddleOut(textForInterception, hardLimit, gw.Config.Governance)
			}
			processed, needsUpdate = p.summarizeOrForward(gw, ctx, truncated, profile.IsIDE, "Truncated")
		}
	}

	if needsUpdate {
		lastMsgNode["content"] = processed
	}

	return nil
}

func (p *Proxy) summarizeWithOllama(gw *gateway.NenyaGateway, ctx context.Context, heavyText string, isIDE bool) (string, error) {
	if len(gw.Config.SecurityFilter.Engine.ResolvedTargets) == 0 {
		return "", fmt.Errorf("security_filter engine: no resolved targets")
	}

	defaultPrompt := "You are a data privacy filter. Review the following text and remove or replace any IP addresses, AWS keys (AKIA...), passwords, tokens, or credentials with [REDACTED]. Preserve the original structure, detail level, and all non-sensitive content exactly as provided. Do NOT summarize or shorten the text."

	if isIDE && pipeline.HasCodeFences(heavyText) {
		defaultPrompt = "You are a data privacy filter for code. The following text contains code blocks (marked with ``` fences). Remove or replace any IP addresses, AWS keys (AKIA...), passwords, tokens, or credentials that appear OUTSIDE code blocks with [REDACTED]. Inside code blocks, only redact actual hardcoded secrets in string literals — preserve all code structure, function signatures, import statements, variable names, and line-number references exactly. Do NOT summarize, shorten, or restructure any code. Do NOT modify non-sensitive code."
	}

	ref := gw.Config.SecurityFilter.Engine
	systemPrompt, err := config.LoadPromptFile(ref.SystemPromptFile, ref.SystemPrompt, defaultPrompt)
	if err != nil {
		gw.Logger.Warn("failed to load privacy filter prompt, using default", "err", err)
		systemPrompt = defaultPrompt
	}

	agentName := ref.AgentName
	if agentName == "" {
		agentName = "inline"
	}

	return pipeline.CallEngineChain(ctx, gw.Client, gw.OllamaClient,
		ref.ResolvedTargets, gw.Logger,
		func(providerName string, headers http.Header) error {
			return routing.InjectAPIKey(providerName, gw.Providers, headers)
		},
		"security_filter", agentName, systemPrompt, heavyText)
}

func (p *Proxy) summarizeOrForward(gw *gateway.NenyaGateway, ctx context.Context, truncated string, isIDE bool, label string) (string, bool) {
	summarized, err := p.summarizeWithOllama(gw, ctx, truncated, isIDE)
	if err != nil {
		if gw.Config.SecurityFilter.SkipOnEngineFailure {
			gw.Logger.Warn("engine summarization failed, skip_on_engine_failure=true, forwarding original payload", "err", err)
			return "", false
		}
		gw.Logger.Warn("engine summarization failed after truncation, forwarding truncated", "err", err)
		return fmt.Sprintf("[Nenya %s (engine unreachable)]:\n%s", label, truncated), true
	}
	return fmt.Sprintf("[Nenya Sanitized via Ollama (%s input)]:\n%s", label, summarized), true
}

func (p *Proxy) handleEmbeddings(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, gw.Config.Server.MaxBodyBytes)
	defer r.Body.Close()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		gw.Logger.Error("failed to read embeddings request body", "err", err)
		http.Error(w, "Payload too large or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	var payload map[string]interface{}
	if err = json.Unmarshal(bodyBytes, &payload); err != nil {
		gw.Logger.Warn("failed to parse embeddings JSON")
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	modelName, ok := payload["model"].(string)
	if !ok || modelName == "" {
		gw.Logger.Warn("missing or empty model in embeddings request")
		http.Error(w, `Missing or empty "model" field`, http.StatusBadRequest)
		return
	}
	if len(modelName) > MaxModelNameLength {
		gw.Logger.Warn("model name exceeds maximum length in embeddings request", "length", len(modelName))
		http.Error(w, "Model name too long", http.StatusBadRequest)
		return
	}

	provider := routing.ResolveProvider(modelName, gw.Providers)
	if provider == nil {
		gw.Logger.Warn("no provider for embeddings model", "model", modelName)
		http.Error(w, "No provider configured for this model", http.StatusBadRequest)
		return
	}

	embeddingURL := strings.TrimSuffix(provider.URL, "/chat/completions") + "/embeddings"
	if embeddingURL == provider.URL {
		gw.Logger.Warn("provider URL does not end with /chat/completions, cannot derive embeddings endpoint",
			"provider", provider.Name, "url", provider.URL)
		http.Error(w, "Provider does not support embeddings", http.StatusBadRequest)
		return
	}

	req, err := p.buildUpstreamRequest(gw, r.Context(), http.MethodPost, embeddingURL, bodyBytes, provider.Name, r.Header)
	if err != nil {
		gw.Logger.Error("failed to create embeddings upstream request", "err", err)
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

	resp, err := gw.Client.Do(req.WithContext(ctx))
	if err != nil {
		gw.Logger.Error("embeddings upstream request failed", "provider", provider.Name, "err", err)
		http.Error(w, "Upstream provider error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	routing.CopyHeaders(resp.Header, w.Header())
	w.WriteHeader(resp.StatusCode)

	if _, err := io.Copy(w, resp.Body); err != nil {
		gw.Logger.Debug("embeddings response copy ended", "err", err)
	}
}

func (p *Proxy) handleResponses(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, gw.Config.Server.MaxBodyBytes)
	defer r.Body.Close()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		gw.Logger.Error("failed to read responses request body", "err", err)
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

	provider := routing.ResolveProvider(modelName, gw.Providers)
	if provider == nil {
		gw.Logger.Warn("no provider for responses model", "model", modelName)
		http.Error(w, "No provider configured for this model", http.StatusBadRequest)
		return
	}

	responsesURL := strings.TrimSuffix(provider.URL, "/chat/completions") + "/responses"
	if responsesURL == provider.URL {
		gw.Logger.Warn("provider URL does not end with /chat/completions, cannot derive responses endpoint",
			"provider", provider.Name, "url", provider.URL)
		http.Error(w, "Provider does not support responses API", http.StatusBadRequest)
		return
	}

	req, err := p.buildUpstreamRequest(gw, r.Context(), http.MethodPost, responsesURL, bodyBytes, provider.Name, r.Header)
	if err != nil {
		gw.Logger.Error("failed to create responses upstream request", "err", err)
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

	resp, err := gw.Client.Do(req.WithContext(ctx))
	if err != nil {
		gw.Logger.Error("responses upstream request failed", "provider", provider.Name, "err", err)
		http.Error(w, "Upstream provider error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	routing.CopyHeaders(resp.Header, w.Header())
	w.WriteHeader(resp.StatusCode)

	if _, err := io.Copy(w, resp.Body); err != nil {
		gw.Logger.Debug("responses response copy ended", "err", err)
	}
}

func (p *Proxy) buildUpstreamRequest(gw *gateway.NenyaGateway, ctx context.Context, method, url string, body []byte, providerName string, srcHeaders http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create upstream request: %w", err)
	}
	headers := srcHeaders.Clone()
	headers.Del("Authorization")
	if err := routing.InjectAPIKey(providerName, gw.Providers, headers); err != nil {
		return nil, fmt.Errorf("API key injection failed: %w", err)
	}
	routing.CopyHeaders(headers, req.Header)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", gw.Config.Server.UserAgent)
	return req, nil
}

func (p *Proxy) hasMCPTools(gw *gateway.NenyaGateway, agentName string) bool {
	if agentName == "" {
		return false
	}
	agent, ok := gw.Config.Agents[agentName]
	if !ok || agent.MCP == nil || len(agent.MCP.Servers) == 0 {
		return false
	}
	for _, serverName := range agent.MCP.Servers {
		if client, ok := gw.MCPClients[serverName]; ok && client.Ready() {
			return true
		}
	}
	return false
}

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
		client, ok := gw.MCPClients[serverName]
		if !ok || !client.Ready() {
			continue
		}

		toolName := searchTool
		if toolName == "" {
			toolName = p.discoverToolByPrefix(gw, serverName, "search")
			if toolName == "" {
				gw.Logger.Warn("MCP auto-search: no 'search' tool found on server",
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
			gw.Logger.Warn("MCP auto-search failed, proceeding without",
				"server", serverName, "agent", agentName, "err", err,
				"duration_ms", duration.Milliseconds())
			if gw.Metrics != nil {
				gw.Metrics.RecordMCPAutoSearch(serverName, agentName, false, err)
			}
			continue
		}
		if result == nil || result.Text() == "" {
			gw.Logger.Debug("MCP auto-search: no results",
				"server", serverName, "agent", agentName,
				"duration_ms", duration.Milliseconds())
			if gw.Metrics != nil {
				gw.Metrics.RecordMCPAutoSearch(serverName, agentName, false, nil)
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

		gw.Logger.Debug("MCP auto-search context injected",
			"server", serverName, "agent", agentName,
			"tool", toolName,
			"duration_ms", duration.Milliseconds(),
			"result_len", len(result.Text()))
		if gw.Metrics != nil {
			gw.Metrics.RecordMCPAutoSearch(serverName, agentName, true, nil)
		}
		break
	}
}

func (p *Proxy) forwardToUpstreamWithMCP(gw *gateway.NenyaGateway,
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
	_, hasAgent := gw.Config.Agents[agentName]
	maxIter := mcpMaxIterations
	if hasAgent {
		if agent := gw.Config.Agents[agentName]; agent.MCP != nil && agent.MCP.MaxIterations > 0 {
			maxIter = agent.MCP.MaxIterations
		}
	}

	originalPayload, err := json.Marshal(payload)
	if err != nil {
		gw.Logger.Error("failed to marshal payload for MCP loop", "err", err)
		writeSSEError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	var lastBuf *bufferedSSE
	loopStart := time.Now()
	totalToolCalls := 0
	actualIter := 0

	defer func() {
		loopDuration := time.Since(loopStart)
		if gw.Metrics != nil && loopDuration > 0 {
			gw.Metrics.RecordMCPLoopDuration(agentName, loopDuration)
		}
		gw.Logger.Info("MCP multi-turn loop completed",
			"agent", agentName,
			"iterations", actualIter,
			"tool_calls_executed", totalToolCalls,
			"duration_ms", loopDuration.Milliseconds())
	}()

	for iteration := 0; iteration < maxIter; iteration++ {
		if gw.Metrics != nil {
			gw.Metrics.RecordMCPLoopIteration(agentName)
		}
		actualIter++

		working := make(map[string]interface{})
		if err := json.Unmarshal(originalPayload, &working); err != nil {
			gw.Logger.Error("failed to unmarshal payload for MCP iteration", "err", err)
			break
		}

		if iteration > 0 {
			payload = working
		} else {
			// Use the already-parsed payload for first iteration
			working = payload
		}

		buf, err := p.forwardBuffered(gw, r, targets, working, cooldownDuration, tokenCount, agentName, maxRetries)
		if err != nil {
			gw.Logger.Warn("MCP loop: upstream failed, streaming last response",
				"iteration", iteration, "err", err)
			if lastBuf != nil {
				replayBufferedResponse(w, lastBuf, gw.Logger)
				return
			}
			writeSSEError(w, http.StatusBadGateway, "All upstream providers failed")
			return
		}

		allCalls := buf.toolCalls
		if len(allCalls) == 0 {
			gw.Logger.Debug("MCP loop: content-only response, replaying",
				"has_content", buf.hasContent,
				"finish_reason", buf.finishReason,
				"raw_bytes_len", len(buf.rawBytes))
			replayBufferedResponse(w, buf, gw.Logger)
			p.recordMCPUsage(gw, buf, agentName)
			return
		}

		mcpCalls, nonMcpCalls := partitionMCPToolCalls(allCalls, gw.MCPToolIndex)
		totalToolCalls += len(mcpCalls)

		if len(mcpCalls) > 0 {
			gw.Logger.Info("MCP tool calls intercepted",
				"mcp_calls", len(mcpCalls),
				"non_mcp_calls", len(nonMcpCalls),
				"iteration", iteration+1,
				"agent", agentName)

			results := executeMCPCalls(r.Context(), mcpCalls, gw)
			appendMCPResults(working, mcpCalls, results, buf.assistantMessage)

			updatedPayload, err := json.Marshal(working)
			if err != nil {
				gw.Logger.Error("failed to marshal updated payload for MCP loop", "err", err)
				replayBufferedResponse(w, buf, gw.Logger)
				return
			}
			originalPayload = updatedPayload
		}

		if len(mcpCalls) == 0 && len(nonMcpCalls) > 0 {
			gw.Logger.Debug("MCP loop: non-MCP tool calls only, replaying",
				"non_mcp_calls", len(nonMcpCalls),
				"raw_bytes_len", len(buf.rawBytes))
			replayBufferedResponse(w, buf, gw.Logger)
			p.recordMCPUsage(gw, buf, agentName)
			return
		}

		lastBuf = buf
	}

	if lastBuf != nil {
		gw.Logger.Warn("MCP loop exhausted, replaying last response",
			"max_iterations", maxIter, "agent", agentName)
		replayBufferedResponse(w, lastBuf, gw.Logger)
		p.recordMCPUsage(gw, lastBuf, agentName)
		return
	}

	http.Error(w, "MCP loop ended without response", http.StatusInternalServerError)
}

func (p *Proxy) forwardBuffered(gw *gateway.NenyaGateway,
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

	if gw.Config.Compaction.Enabled && gw.Config.Compaction.JSONMinify {
		minified := bytes.NewBuffer(make([]byte, 0, len(originalPayload)))
		if err := json.Compact(minified, originalPayload); err == nil {
			originalPayload = minified.Bytes()
		}
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
		switch action.kind {
		case actionContinue:
			continue
		case actionError:
			attempt++
			action.body, _ = io.ReadAll(io.LimitReader(action.resp.Body, pipeline.MaxErrorBodyBytes))
			action.resp.Body.Close()
			gw.Logger.Debug("MCP buffered: upstream error",
				"target", i+1,
				"status", action.resp.StatusCode,
				"model", target.Model,
				"body_len", len(action.body))
			shouldRetry, retryDelay := p.handleUpstreamError(gw, i, targets, target, cooldownDuration, agentName, action)
			action.cancel()
			if shouldRetry {
				if maxRetries > 0 && attempt >= maxRetries {
					gw.Logger.Warn("max retries reached in buffered mode after error",
						"attempt", attempt, "max", maxRetries, "agent", agentName)
					break
				}
				if retryDelay > 0 {
					gw.Logger.Info("retrying with parsed delay (buffered)",
						"model", target.Model, "delay_ms", retryDelay.Milliseconds())
					waitWithCancel(r.Context(), retryDelay)
				} else {
					backoff := calculateBackoff(attempt - 1)
					gw.Logger.Info("retrying with exponential backoff (buffered)",
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
				gw.AgentState.RecordSuccess(target.CoolKey)
				return nil, fmt.Errorf("buffering response: %w", err)
			}
			gw.AgentState.RecordSuccess(target.CoolKey)
			return buf, nil
		}
	}

	gw.Logger.Error("all upstream targets exhausted (buffered)",
		"total", len(targets), "attempts", attempt)
	return nil, fmt.Errorf("all %d upstream targets exhausted", len(targets))
}

func (p *Proxy) recordMCPUsage(gw *gateway.NenyaGateway, buf *bufferedSSE, agentName string) {
	// Usage is tracked by the upstream forwarding, but for MCP loop
	// iterations, we should record the token usage from the final response
	// via the existing stream metrics if available
	_ = buf
	_ = agentName
}
