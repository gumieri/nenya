package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"nenya/internal/config"
	"nenya/internal/gateway"
	"nenya/internal/infra"
	"nenya/internal/mcp"
	"nenya/internal/pipeline"
	"nenya/internal/routing"
	"nenya/internal/util"
	"net/http"
	"strings"
	"time"
)

// MCP timeout constants for automatic operations.
const (
	mcpAutoSearchTimeout        = 10 * time.Second
	mcpLoopMaxDuration          = 5 * time.Minute
	mcpMaxIterations            = 10
	mcpMaxIterationsHardCeiling = 50
)

// chatRequest holds the validated request data extracted from an incoming
// /v1/chat/completions payload.
type chatRequest struct {
	Payload      map[string]any
	ModelName    string
	TokenCount   int
	Targets      []routing.UpstreamTarget
	AgentName    string
	Cooldown     time.Duration
	MaxRetries   int
	CacheKey     string
	HasMCPTools  bool
	SoftLimit    int
	HardLimit    int
	WindowMaxCtx int
	Profile      pipeline.ClientProfile
	Messages     []any
}

// httpError pairs an HTTP status code with a user-facing message.
type httpError struct {
	Code    int
	Message string
}

func (e *httpError) Error() string { return e.Message }

// validateChatRequest reads and validates the incoming request body,
// returning a populated chatRequest or an httpError.
func (p *Proxy) validateChatRequest(w http.ResponseWriter, r *http.Request, gw *gateway.NenyaGateway) (*chatRequest, *httpError) {
	r.Body = http.MaxBytesReader(w, r.Body, gw.Config.Server.MaxBodyBytes)

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		gw.Logger.Error("failed to read request body", "err", err)
		return nil, &httpError{http.StatusRequestEntityTooLarge, "Payload too large or malformed"}
	}
	defer func() { _ = r.Body.Close() }()

	if r.Context().Err() != nil {
		return nil, &httpError{http.StatusRequestEntityTooLarge, "Request canceled"}
	}

	var payload map[string]any
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		gw.Logger.Warn("failed to parse JSON, returning Bad Request")
		return nil, &httpError{http.StatusBadRequest, "Invalid JSON payload"}
	}

	modelName, ok := payload["model"].(string)
	if !ok || modelName == "" {
		gw.Logger.Warn("missing or empty model field in request body")
		return nil, &httpError{http.StatusBadRequest, `Missing or empty "model" field in request body`}
	}
	if len(modelName) > MaxModelNameLength {
		gw.Logger.Warn("model name exceeds maximum length", "length", len(modelName))
		return nil, &httpError{http.StatusBadRequest, "Model name too long"}
	}

	req := &chatRequest{
		Payload:    payload,
		ModelName:  modelName,
		TokenCount: gw.CountRequestTokens(payload),
	}

	var herr *httpError
	req.Targets, req.AgentName, req.Cooldown, req.MaxRetries, herr = p.resolveRouting(req, gw)
	if herr != nil {
		return nil, herr
	}

	req.CacheKey, herr = p.resolveCache(w, r, gw, req)
	if herr != nil {
		return nil, herr
	}

	req.Messages, req.HasMCPTools, req.SoftLimit, req.HardLimit, req.WindowMaxCtx, req.Profile = p.resolvePipelineContext(r, gw, req)

	return req, nil
}

// resolveRouting determines the upstream targets, agent name, cooldown, and
// max retries for the given model.
func (p *Proxy) resolveRouting(req *chatRequest, gw *gateway.NenyaGateway) ([]routing.UpstreamTarget, string, time.Duration, int, *httpError) {
	if agent, ok := gw.Config.Agents[req.ModelName]; ok {
		req.AgentName = req.ModelName
		secs := agent.CooldownSeconds
		if secs == 0 {
			secs = routing.DefaultAgentCooldownSec
		}
		cooldown := time.Duration(secs) * time.Second
		maxRetries := agent.MaxRetries

		targets := gw.AgentState.BuildTargetList(gw.Logger, req.ModelName, agent, req.TokenCount, gw.Providers, gw.ModelCatalog, gw.Config.Governance.AutoContextSkip)
		if len(targets) == 0 {
			if len(agent.Models) > 0 {
				gw.Logger.Warn("all models excluded by max_context",
					"agent", req.ModelName, "tokens", req.TokenCount)
				return nil, "", 0, 0, &httpError{http.StatusRequestEntityTooLarge, "Request too large for all configured models in this agent"}
			}
			gw.Logger.Error("agent has no models configured", "agent", req.ModelName)
			return nil, "", 0, 0, &httpError{http.StatusInternalServerError, "Agent has no models configured"}
		}

		if gw.Config.Governance.AutoReorderByLatency {
			switch gw.Config.Governance.RoutingStrategy {
			case "balanced":
				targets = routing.SortTargetsByBalanced(targets, gw.LatencyTracker, gw.CostTracker, gw.ModelCatalog, routing.SortOptions{
					LatencyWeight: gw.Config.Governance.RoutingLatencyWeight,
					CostWeight:    gw.Config.Governance.RoutingCostWeight,
					RequestCaps:   detectRequestCapabilities(req.Payload),
				})
			default:
				targets = routing.SortTargetsByLatency(targets, gw.LatencyTracker, nil)
			}
		}

		strategy := agent.Strategy
		if strategy == "" {
			strategy = "round-robin"
		}
		gw.Logger.Info("agent routing",
			"agent", req.ModelName, "strategy", strategy, "models_in_chain", len(targets))

		return targets, req.AgentName, cooldown, maxRetries, nil
	}

	provider := routing.ResolveProvider(req.ModelName, gw.Providers, gw.ModelCatalog)
	if provider == nil {
		gw.Logger.Warn("no provider found for model", "model", req.ModelName)
		return nil, "", 0, 0, &httpError{http.StatusBadRequest, util.ErrNoProvider}
	}
	targets := []routing.UpstreamTarget{{URL: provider.URL, Model: req.ModelName, Provider: provider.Name}}
	gw.Logger.Info("model routing", "model", req.ModelName, "upstream", provider.URL)

	return targets, "", 0, 0, nil
}

// resolveCache checks the response cache and returns a cache key. If a cached
// response is found and served, it returns ("", httpError) to signal early return.
func (p *Proxy) resolveCache(w http.ResponseWriter, r *http.Request, gw *gateway.NenyaGateway, req *chatRequest) (string, *httpError) {
	if gw.ResponseCache == nil {
		return "", nil
	}

	cacheKey := infra.FingerprintPayload(req.Payload)
	if r.Header.Get(gw.Config.ResponseCache.ForceRefreshHeader) == "" {
		if data, ok := gw.ResponseCache.Lookup(cacheKey); ok {
			p.replayCachedSSE(gw, w, r, data)
			return "", &httpError{http.StatusOK, "cache hit"}
		}
	}
	return cacheKey, nil
}

// resolvePipelineContext extracts messages, MCP tool state, limits, and client
// profile from the validated request.
func (p *Proxy) resolvePipelineContext(r *http.Request, gw *gateway.NenyaGateway, req *chatRequest) ([]any, bool, int, int, int, pipeline.ClientProfile) {
	messagesRaw, ok := req.Payload["messages"]
	if !ok {
		return nil, false, 4000, 24000, 0, pipeline.ClientProfile{}
	}
	messages, ok := messagesRaw.([]any)
	if !ok || len(messages) == 0 {
		gw.Logger.Warn("messages field is not a non-empty array, skipping Ollama interception")
		return nil, false, 4000, 24000, 0, pipeline.ClientProfile{}
	}

	autoSearchCtx, autoSearchCancel := context.WithTimeout(r.Context(), mcpAutoSearchTimeout)
	p.injectAutoSearch(gw, autoSearchCtx, req.Payload, messages, req.AgentName)
	autoSearchCancel()
	p.injectMCPTools(gw, req.Payload, req.AgentName)

	softLimit := 4000
	hardLimit := 24000
	if len(req.Targets) > 0 {
		primaryTarget := req.Targets[0]
		if primaryTarget.MaxContext > 0 {
			softLimit = primaryTarget.MaxContext / 8
			hardLimit = primaryTarget.MaxContext * 3 / 4
		}
	}

	windowMaxCtx := routing.ResolveWindowMaxContext(req.ModelName, gw.Config.Agents, gw.ModelCatalog)
	profile := pipeline.ClassifyClient(r.Header)
	if profile.IsIDE {
		gw.Logger.Debug("IDE client detected", "client", profile.ClientName)
	}

	return messages, p.hasMCPTools(gw, req.AgentName), softLimit, hardLimit, windowMaxCtx, profile
}

// handleChatCompletions processes chat completion requests with optional content filtering and tool integration.
func (p *Proxy) handleChatCompletions(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	req, herr := p.validateChatRequest(w, r, gw)
	if herr != nil {
		if herr.Code == http.StatusNoContent {
			return
		}
		http.Error(w, herr.Message, herr.Code)
		return
	}

	if err := p.applyContentPipeline(gw, r.Context(), req.Payload, req.TokenCount, req.WindowMaxCtx, req.Profile, req.SoftLimit, req.HardLimit); err != nil {
		gw.Logger.Warn("content pipeline failed, proceeding with original payload", "err", err)
	}

	if req.HasMCPTools {
		p.forwardToUpstreamWithMCP(gw, w, r, forwardOptions{
			Targets:    req.Targets,
			Payload:    req.Payload,
			Cooldown:   req.Cooldown,
			TokenCount: req.TokenCount,
			AgentName:  req.AgentName,
			MaxRetries: req.MaxRetries,
			CacheKey:   req.CacheKey,
		})
		return
	}

	p.forwardToUpstream(gw, w, r, forwardOptions{
		Targets:    req.Targets,
		Payload:    req.Payload,
		Cooldown:   req.Cooldown,
		TokenCount: req.TokenCount,
		AgentName:  req.AgentName,
		MaxRetries: req.MaxRetries,
		CacheKey:   req.CacheKey,
	})
}

// replayCachedSSE serves a previously cached response using Server-Sent Events.
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

	if !profile.IsIDE {
		if pipeline.ApplyCompaction(messages, gw.Config.Compaction) {
			gw.Metrics.RecordCompaction()
		}
	}

	applyPatternRedaction(gw, messages)
	applyEntropyRedaction(gw, messages)

	messages = payload["messages"].([]interface{})
	if len(messages) == 0 {
		return nil
	}
	if !profile.IsIDE {
		if pipeline.PruneStaleToolCalls(payload, gw.Config.Compaction) {
			gw.Metrics.RecordCompaction()
		}
		if pipeline.PruneThoughts(payload, gw.Config.Compaction) {
			gw.Metrics.RecordCompaction()
		}
	}

	messages = payload["messages"].([]interface{})
	if len(messages) == 0 {
		return nil
	}
	deps := buildWindowDeps(gw)
	if windowed, err := pipeline.ApplyWindowCompaction(ctx, deps, payload, messages, tokenCount, gw.Config.Window, windowMaxCtx, gw.CountRequestTokens); err != nil {
		gw.Logger.Warn("window compaction failed, proceeding without it", "err", err)
	} else if windowed {
		gw.Metrics.RecordWindow(gw.Config.Window.Mode)
	}

	messages = payload["messages"].([]interface{})
	if len(messages) == 0 {
		return nil
	}
	return p.interceptContent(gw, ctx, messages, payload, profile, softLimit, hardLimit)
}

// buildWindowDeps creates a WindowDeps from the gateway state.
func buildWindowDeps(gw *gateway.NenyaGateway) pipeline.WindowDeps {
	return pipeline.WindowDeps{
		Logger:       gw.Logger,
		Client:       gw.Client,
		OllamaClient: gw.OllamaClient,
		Providers:    gw.Providers,
		InjectAPIKey: func(providerName string, headers http.Header) error {
			return routing.InjectAPIKey(providerName, gw.Providers, headers)
		},
		CountTokens: gw.CountTokens,
	}
}

// applyPatternRedaction runs pattern-based secret redaction on all messages.
func applyPatternRedaction(gw *gateway.NenyaGateway, messages []interface{}) {
	patternRedacted := false
	for _, msgRaw := range messages {
		msgNode, isMap := msgRaw.(map[string]interface{})
		if !isMap {
			continue
		}
		if pipeline.ShouldSkipRedaction(msgNode, gw.Config.PrefixCache) {
			continue
		}
		if applyRedactToContent(msgNode, func(s string) string {
			return pipeline.RedactSecrets(s, gw.Config.SecurityFilter.Enabled, gw.SecretPatterns, gw.Config.SecurityFilter.RedactionLabel)
		}) {
			patternRedacted = true
		}
	}
	if patternRedacted {
		gw.Metrics.RecordRedaction()
	}
}

// applyEntropyRedaction runs entropy-based high-entropy string redaction on all
// messages when an entropy filter is configured.
func applyEntropyRedaction(gw *gateway.NenyaGateway, messages []interface{}) {
	if gw.EntropyFilter == nil {
		return
	}
	entropyRedacted := false
	for _, msgRaw := range messages {
		msgNode, isMap := msgRaw.(map[string]interface{})
		if !isMap {
			continue
		}
		if pipeline.ShouldSkipRedaction(msgNode, gw.Config.PrefixCache) {
			continue
		}
		if applyRedactToContent(msgNode, func(s string) string {
			return gw.EntropyFilter.RedactHighEntropy(s, gw.Config.SecurityFilter.RedactionLabel)
		}) {
			entropyRedacted = true
		}
	}
	if entropyRedacted {
		gw.Metrics.RecordRedaction()
	}
}

// interceptContent extracts the last user message and applies the content
// interception pipeline (soft limit → Ollama engine, hard limit → truncate +
// engine with optional TF-IDF scoring).
func (p *Proxy) interceptContent(gw *gateway.NenyaGateway, ctx context.Context, messages []interface{}, payload map[string]interface{}, profile pipeline.ClientProfile, softLimit, hardLimit int) error {
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

	contentTokens := gw.CountTokens(textForInterception)

	var processed string
	var needsUpdate bool

	if contentTokens < softLimit {
		gw.Logger.Debug("payload within soft limit, passing through",
			"tokens", contentTokens, "soft_limit", softLimit)
	} else if contentTokens <= hardLimit {
		processed, needsUpdate = p.interceptSoftLimit(gw, ctx, textForInterception, profile.IsIDE)
	} else {
		processed, needsUpdate = p.interceptHardLimit(gw, ctx, textForInterception, messages, profile, softLimit, hardLimit, contentTokens)
	}

	if needsUpdate {
		lastMsgNode["content"] = processed
	}
	return nil
}

// interceptSoftLimit handles the case where content tokens exceed the soft
// limit but are within the hard limit: send to engine for summarization.
func (p *Proxy) interceptSoftLimit(gw *gateway.NenyaGateway, ctx context.Context, text string, isIDE bool) (string, bool) {
	gw.Logger.Warn("payload exceeds soft limit, sending to engine",
		"tokens", gw.CountTokens(text))
	gw.Metrics.RecordInterception("soft_limit")
	summarized, err := p.summarizeWithOllama(gw, ctx, text, isIDE)
	if err != nil {
		gw.Logger.Warn("engine summarization failed, proceeding with original payload", "err", err)
		return "", false
	}
	return fmt.Sprintf("[Nenya Sanitized via Ollama]:\n%s", summarized), true
}

// interceptHardLimit handles the case where content tokens exceed the hard
// limit: truncate first, then optionally apply TF-IDF relevance scoring before
// sending to the engine.
func (p *Proxy) interceptHardLimit(gw *gateway.NenyaGateway, ctx context.Context, text string, messages []interface{}, profile pipeline.ClientProfile, softLimit, hardLimit, contentTokens int) (string, bool) {
	gw.Logger.Warn("payload exceeds hard limit, truncating before engine",
		"tokens", contentTokens, "hard_limit", hardLimit)
	gw.Metrics.RecordInterception("hard_limit")

	hardLimitRunes := hardLimit * 3
	querySource := gw.Config.Governance.TFIDFQuerySource

	if querySource != "" {
		return p.interceptWithTFIDF(gw, ctx, text, messages, profile, softLimit, hardLimitRunes, contentTokens, querySource)
	}

	return p.interceptWithMiddleOut(gw, ctx, text, profile, hardLimitRunes)
}

// interceptWithTFIDF applies TF-IDF relevance truncation, then optionally sends
// to engine if still above the soft limit.
func (p *Proxy) interceptWithTFIDF(gw *gateway.NenyaGateway, ctx context.Context, text string, messages []interface{}, profile pipeline.ClientProfile, softLimit, hardLimitRunes, contentTokens int, querySource string) (string, bool) {
	var query string
	switch querySource {
	case "prior_messages":
		query = pipeline.ExtractPriorUserMessages(messages[:len(messages)-1], 5)
	case "self":
		query = pipeline.ExtractSelfQuery(text, 500)
	}
	gw.Logger.Info("TF-IDF truncation enabled",
		"query_source", querySource,
		"input_tokens", contentTokens)

	var truncated string
	if profile.IsIDE {
		truncated = pipeline.TruncateTFIDFCodeAware(text, hardLimitRunes, query, gw.Config.Governance)
	} else {
		truncated = pipeline.TruncateTFIDF(text, hardLimitRunes, query, gw.Config.Governance)
	}

	if gw.CountTokens(truncated) < softLimit {
		gw.Logger.Info("TF-IDF reduced payload below soft limit, skipping engine",
			"soft_limit", softLimit)
		return fmt.Sprintf("[Nenya TF-IDF Pruned]:\n%s", truncated), true
	}

	return p.summarizeOrForward(gw, ctx, truncated, profile.IsIDE, "TF-IDF Pruned")
}

// interceptWithMiddleOut applies middle-out truncation, then sends to the
// engine.
func (p *Proxy) interceptWithMiddleOut(gw *gateway.NenyaGateway, ctx context.Context, text string, profile pipeline.ClientProfile, hardLimitRunes int) (string, bool) {
	var truncated string
	if profile.IsIDE {
		truncated = pipeline.TruncateMiddleOutCodeAware(text, hardLimitRunes, gw.Config.Governance)
	} else {
		truncated = pipeline.TruncateMiddleOut(text, hardLimitRunes, gw.Config.Governance)
	}
	return p.summarizeOrForward(gw, ctx, truncated, profile.IsIDE, "Truncated")
}

// summarizeWithOllama sends content to the security filter engine for redaction and summarization.
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

// summarizeOrForward attempts engine summarization with fallback to raw content if engine fails.
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
	defer func() { _ = r.Body.Close() }()

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

	provider := routing.ResolveProvider(modelName, gw.Providers, gw.ModelCatalog)
	if provider == nil {
		gw.Logger.Warn("no provider for embeddings model", "model", modelName)
		http.Error(w, util.ErrNoProvider, http.StatusBadRequest)
		return
	}

	embeddingURL := strings.TrimSuffix(provider.URL, "/chat/completions") + "/embeddings"
	if embeddingURL == provider.URL {
		gw.Logger.Warn("provider URL does not end with /chat/completions, cannot derive embeddings endpoint",
			"provider", provider.Name, "url", provider.URL)
		http.Error(w, "Provider does not support embeddings", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if provider.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(r.Context(), time.Duration(provider.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	req, err := p.buildUpstreamRequest(gw, ctx, http.MethodPost, embeddingURL, bodyBytes, provider.Name, r.Header)
	if err != nil {
		gw.Logger.Error("failed to create embeddings upstream request", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := gw.Client.Do(req)
	if err != nil {
		gw.Logger.Error("embeddings upstream request failed", "provider", provider.Name, "err", err)
		http.Error(w, "Upstream provider error", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	routing.CopyHeaders(resp.Header, w.Header())
	w.WriteHeader(resp.StatusCode)

	if _, err := copyStream(ctx, w, resp.Body, nil); err != nil {
		gw.Logger.Debug("embeddings response copy ended", "err", err)
	}
}

func (p *Proxy) handleResponses(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, gw.Config.Server.MaxBodyBytes)
	defer func() { _ = r.Body.Close() }()

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

	provider := routing.ResolveProvider(modelName, gw.Providers, gw.ModelCatalog)
	if provider == nil {
		gw.Logger.Warn("no provider for responses model", "model", modelName)
		http.Error(w, util.ErrNoProvider, http.StatusBadRequest)
		return
	}

	responsesURL := strings.TrimSuffix(provider.URL, "/chat/completions") + "/responses"
	if responsesURL == provider.URL {
		gw.Logger.Warn("provider URL does not end with /chat/completions, cannot derive responses endpoint",
			"provider", provider.Name, "url", provider.URL)
		http.Error(w, "Provider does not support responses API", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if provider.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(r.Context(), time.Duration(provider.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	req, err := p.buildUpstreamRequest(gw, ctx, http.MethodPost, responsesURL, bodyBytes, provider.Name, r.Header)
	if err != nil {
		gw.Logger.Error("failed to create responses upstream request", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := gw.Client.Do(req)
	if err != nil {
		gw.Logger.Error("responses upstream request failed", "provider", provider.Name, "err", err)
		http.Error(w, "Upstream provider error", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	routing.CopyHeaders(resp.Header, w.Header())
	w.WriteHeader(resp.StatusCode)

	if _, err := copyStream(ctx, w, resp.Body, nil); err != nil {
		gw.Logger.Debug("responses response copy ended", "err", err)
	}
}

func (p *Proxy) buildUpstreamRequest(gw *gateway.NenyaGateway, ctx context.Context, method, url string, body []byte, providerName string, srcHeaders http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create upstream request: %w", err)
	}
	if err := routing.InjectAPIKey(providerName, gw.Providers, req.Header); err != nil {
		return nil, fmt.Errorf("API key injection failed: %w", err)
	}
	// Forward only safe passthrough headers; never let client-supplied
	// headers leak internal routing tokens or override upstream auth.
	for _, h := range []string{
		"X-Request-Id", "X-Correlation-Id", "X-Trace-Id",
		"Traceparent", "Tracestate",
	} {
		if v := srcHeaders.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
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
	query = pipeline.RedactSecrets(query, gw.Config.SecurityFilter.Enabled, gw.SecretPatterns, gw.Config.SecurityFilter.RedactionLabel)
	if gw.EntropyFilter != nil {
		query = gw.EntropyFilter.RedactHighEntropy(query, gw.Config.SecurityFilter.RedactionLabel)
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
	resultText = pipeline.RedactSecrets(resultText, gw.Config.SecurityFilter.Enabled, gw.SecretPatterns, gw.Config.SecurityFilter.RedactionLabel)
	if gw.EntropyFilter != nil {
		resultText = gw.EntropyFilter.RedactHighEntropy(resultText, gw.Config.SecurityFilter.RedactionLabel)
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

	for iteration := 0; iteration < maxIter; iteration++ {
		select {
		case <-mcpLoopCtx.Done():
			gw.Logger.Warn("MCP loop deadline exceeded", "agent", opts.AgentName, "iterations", actualIter)
			if lastBuf != nil {
				replayBufferedResponse(w, lastBuf, gw.Logger)
			} else {
				writeSSEError(w, http.StatusRequestTimeout, "MCP loop deadline exceeded")
			}
			return
		default:
		}

		gw.Metrics.RecordMCPLoopIteration(opts.AgentName)
		actualIter++

		working := make(map[string]interface{})
		if err := json.Unmarshal(originalPayload, &working); err != nil {
			gw.Logger.Error("failed to unmarshal payload for MCP iteration", "err", err)
			break
		}

		if iteration > 0 {
			// Use the unmarshaled copy for subsequent iterations
		} else {
			// Use the already-parsed opts.Payload for first iteration
			working = opts.Payload
		}

		buf, err := p.forwardBuffered(gw, mcpLoopCtx, r, opts.Targets, working, opts.Cooldown, opts.TokenCount, opts.AgentName, opts.MaxRetries)
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
			p.recordMCPUsage(gw, buf, opts.AgentName)
			return
		}

		mcpCalls, nonMcpCalls := partitionMCPToolCalls(allCalls, gw.MCPToolIndex)
		totalToolCalls += len(mcpCalls)

		if len(mcpCalls) > 0 {
			gw.Logger.Info("MCP tool calls intercepted",
				"mcp_calls", len(mcpCalls),
				"non_mcp_calls", len(nonMcpCalls),
				"iteration", iteration+1,
				"agent", opts.AgentName)

			results := executeMCPCalls(mcpLoopCtx, mcpCalls, gw, opts.AgentName)
			// Build an assistant message that only lists the MCP calls being
			// handled. Using buf.assistantMessage (which contains ALL calls)
			// would create orphaned tool_call references if nonMcpCalls is
			// non-empty, causing providers like Ollama to reject the payload.
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
			p.recordMCPUsage(gw, buf, opts.AgentName)
			return
		}

		lastBuf = buf
	}

	if lastBuf != nil {
		gw.Logger.Warn("MCP loop exhausted, replaying last response",
			"max_iterations", maxIter, "agent", opts.AgentName)
		replayBufferedResponse(w, lastBuf, gw.Logger)
		p.recordMCPUsage(gw, lastBuf, opts.AgentName)
		return
	}

	http.Error(w, "MCP loop ended without response", http.StatusInternalServerError)
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
			_ = action.resp.Body.Close()
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
					waitWithCancel(ctx, retryDelay)
				} else {
					backoff := calculateBackoff(attempt - 1)
					gw.Logger.Info("retrying with exponential backoff (buffered)",
						"model", target.Model, "attempt", attempt, "delay_ms", backoff.Milliseconds())
					waitWithCancel(ctx, backoff)
				}
				continue
			}
			return nil, fmt.Errorf("upstream error: status %d", action.resp.StatusCode)
		case actionStream:
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

// applyRedactToContent runs redactFn against every text surface of msgNode's
// content, preserving multimodal content arrays instead of flattening them to
// a string. Returns true if any part was changed.
func detectRequestCapabilities(payload map[string]interface{}) routing.RequestCapabilities {
	var caps routing.RequestCapabilities

	if tools, ok := payload["tools"].([]interface{}); ok && len(tools) > 0 {
		caps.HasToolCalls = true
	}

	if messages, ok := payload["messages"].([]interface{}); ok {
		for _, msg := range messages {
			m, ok := msg.(map[string]interface{})
			if !ok {
				continue
			}
			content := m["content"]
			if arr, ok := content.([]interface{}); ok && len(arr) > 0 {
				caps.HasContentArr = true
				for _, part := range arr {
					if p, ok := part.(map[string]interface{}); ok {
						if t, ok := p["type"].(string); ok && t == "image_url" {
							caps.HasVision = true
							break
						}
					}
				}
			}
			if reasoning, ok := m["reasoning"].(map[string]interface{}); ok && len(reasoning) > 0 {
				caps.HasReasoning = true
			}
			if caps.HasVision && caps.HasReasoning {
				break
			}
		}
	}

	return caps
}

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
