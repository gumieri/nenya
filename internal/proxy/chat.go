package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/adapter"
	"github.com/nenya/internal/billing"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/util"
)

// chatRequest holds the validated request data extracted from an incoming
// /v1/chat/completions or /v1/messages payload.
type chatRequest struct {
	Payload      map[string]any
	ModelName    string
	TokenCount   int
	Stream       bool
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
	KeyRef       string
	SourceFormat string // "openai" or "anthropic" - indicates the original client request format
}

// httpError pairs an HTTP status code with a user-facing message.
type httpError struct {
	Code    int
	Message string
}

func (e *httpError) Error() string { return e.Message }

// parseRequestBody parses the request body and converts it to OpenAI format if needed.
// Returns the parsed payload, source format ("openai" or "anthropic"), and an error if parsing fails.
func (p *Proxy) parseRequestBody(gw *gateway.NenyaGateway, r *http.Request, bodyBytes []byte) (map[string]any, string, *httpError) {
	sourceFormat := "openai"
	if _, hasType := routing.ExtractField(bodyBytes, "type"); hasType {
		converted, err := routing.TransformIncomingAnthropicRequest(r.Context(), bodyBytes)
		if err != nil {
			gw.Logger.Warn("failed to convert Anthropic request", "err", err)
			return nil, "", &httpError{http.StatusBadRequest, "Failed to convert Anthropic format request"}
		}
		if converted != nil && string(converted) != string(bodyBytes) {
			sourceFormat = "anthropic"
			bodyBytes = converted
		}
	}

	var payload map[string]any
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		gw.Logger.Warn("failed to parse JSON, returning Bad Request")
		return nil, "", &httpError{http.StatusBadRequest, "Invalid JSON payload"}
	}
	return payload, sourceFormat, nil
}

// validateChatRequest reads and validates the incoming request body,
// returning a populated chatRequest or an httpError.
func (p *Proxy) validateChatRequest(w http.ResponseWriter, r *http.Request, gw *gateway.NenyaGateway, keyRef string) (*chatRequest, *httpError) {
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

	payload, sourceFormat, herr := p.parseRequestBody(gw, r, bodyBytes)
	if herr != nil {
		return nil, herr
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
		Payload:      payload,
		ModelName:    modelName,
		TokenCount:   gw.CountRequestTokens(payload),
		Stream:       false,
		KeyRef:       keyRef,
		SourceFormat: sourceFormat,
	}
	if streamRaw, ok := payload["stream"]; ok {
		if s, ok := streamRaw.(bool); ok {
			req.Stream = s
		}
	}

	req.Targets, req.AgentName, req.Cooldown, req.MaxRetries, herr = p.resolveRouting(r.Context(), req, gw)
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
func (p *Proxy) resolveRouting(ctx context.Context, req *chatRequest, gw *gateway.NenyaGateway) ([]routing.UpstreamTarget, string, time.Duration, int, *httpError) {
	if agent, ok := gw.Config.Agents[req.ModelName]; ok {
		return p.resolveAgentRouting(ctx, req, gw, agent)
	}
	return p.resolveModelRouting(ctx, req, gw)
}

func (p *Proxy) resolveAgentRouting(ctx context.Context, req *chatRequest, gw *gateway.NenyaGateway, agent config.AgentConfig) ([]routing.UpstreamTarget, string, time.Duration, int, *httpError) {
	req.AgentName = req.ModelName
	cooldown := getAgentCooldown(agent)
	maxRetries := agent.MaxRetries

	targets := gw.AgentState.BuildTargetList(ctx, gw.Logger, req.ModelName, agent, req.TokenCount, gw.Providers, gw.ModelCatalog, gw.Config.Governance.AutoContextSkip != nil && *gw.Config.Governance.AutoContextSkip, gw)
	targets = filterExhaustedTargets(targets, gw.BillingTracker, gw.Logger)
	if len(targets) == 0 {
		return handleEmptyAgentTargets(req, gw, agent)
	}

	if gw.Config.Governance.AutoReorderByLatency != nil && *gw.Config.Governance.AutoReorderByLatency {
		targets = reorderTargetsByLatency(req, gw, targets)
	}

	strategy := agent.Strategy
	if strategy == "" {
		strategy = "round-robin"
	}
	gw.Logger.Info("agent routing",
		"agent", req.ModelName, "strategy", strategy, "models_in_chain", len(targets))

	return targets, req.AgentName, cooldown, maxRetries, nil
}

func getAgentCooldown(agent config.AgentConfig) time.Duration {
	secs := agent.CooldownSeconds
	if secs == 0 {
		secs = routing.DefaultAgentCooldownSec
	}
	return time.Duration(secs) * time.Second
}

func handleEmptyAgentTargets(req *chatRequest, gw *gateway.NenyaGateway, agent config.AgentConfig) ([]routing.UpstreamTarget, string, time.Duration, int, *httpError) {
	if len(agent.Models) > 0 {
		gw.Logger.Warn("all models excluded by max_context",
			"agent", req.ModelName, "tokens", req.TokenCount)
		return nil, "", 0, 0, &httpError{http.StatusRequestEntityTooLarge, "Request too large for all configured models in this agent"}
	}
	gw.Logger.Error("agent has no models configured", "agent", req.ModelName)
	return nil, "", 0, 0, &httpError{http.StatusInternalServerError, "Agent has no models configured"}
}

func reorderTargetsByLatency(req *chatRequest, gw *gateway.NenyaGateway, targets []routing.UpstreamTarget) []routing.UpstreamTarget {
	switch gw.Config.Governance.RoutingStrategy {
	case "balanced":
		return routing.SortTargetsByBalanced(targets, gw.LatencyTracker, gw.CostTracker, gw.ModelCatalog, routing.SortOptions{
			LatencyWeight:     gw.Config.Governance.RoutingLatencyWeight,
			CostWeight:        gw.Config.Governance.RoutingCostWeight,
			BillingMode:       routing.BillingMode(gw.Config.Governance.CostMode),
			BillingEconomy:    gw.Config.Governance.BillingEconomyScale,
			BillingQuality:    gw.Config.Governance.BillingQualityScale,
			BillingModel:      collectProviderBillingModels(gw.Providers),
			BillingFreeOnly:   collectProviderFreeOnly(gw.Providers),
			BillingFreeModels: collectProviderFreeModels(gw.Providers),
			RequestCaps:       detectRequestCapabilities(req.Payload),
		})
	default:
		return routing.SortTargetsByLatency(targets, gw.LatencyTracker, nil)
	}
}

func collectProviderBillingModels(providers map[string]*config.Provider) map[string]string {
	result := make(map[string]string)
	for name, p := range providers {
		if p.Billing != nil && p.Billing.Model != "" {
			result[name] = string(p.Billing.Model)
		}
	}
	return result
}

func collectProviderFreeOnly(providers map[string]*config.Provider) map[string]bool {
	result := make(map[string]bool)
	for name, p := range providers {
		if p.Billing != nil && p.Billing.FreeOnly {
			result[name] = true
		}
	}
	return result
}

func collectProviderFreeModels(providers map[string]*config.Provider) map[string][]string {
	result := make(map[string][]string)
	for name, p := range providers {
		if p.Billing != nil && len(p.Billing.FreeModels) > 0 {
			result[name] = p.Billing.FreeModels
		}
	}
	return result
}

func filterExhaustedTargets(targets []routing.UpstreamTarget, tracker *billing.BillingTracker, logger *slog.Logger) []routing.UpstreamTarget {
	if tracker == nil || len(targets) == 0 {
		return targets
	}
	filtered := make([]routing.UpstreamTarget, 0, len(targets))
	for _, t := range targets {
		if tracker.IsExhausted(t.Provider, t.AccountName) {
			logger.Debug("skipping exhausted billing account",
				"provider", t.Provider, "account", t.AccountName, "model", t.Model)
			continue
		}
		filtered = append(filtered, t)
	}
	return filtered
}

func (p *Proxy) resolveModelRouting(ctx context.Context, req *chatRequest, gw *gateway.NenyaGateway) ([]routing.UpstreamTarget, string, time.Duration, int, *httpError) {
	matches := routing.ResolveProviders(req.ModelName, gw.Providers, gw.ModelCatalog)
	if len(matches) == 0 {
		gw.Logger.Warn("no provider found for model", "model", req.ModelName)
		return nil, "", 0, 0, &httpError{http.StatusBadRequest, util.ErrNoProvider}
	}

	targets := buildProviderTargets(ctx, matches, gw, gw)
	targets = filterExhaustedTargets(targets, gw.BillingTracker, gw.Logger)
	if len(targets) == 0 {
		gw.Logger.Error("no valid providers after filtering", "model", req.ModelName)
		return nil, "", 0, 0, &httpError{http.StatusInternalServerError, "No valid providers available"}
	}

	gw.Logger.Info("model routing", "model", req.ModelName, "providers", len(targets), "upstreams", targets)
	return targets, "", 0, 0, nil
}

func buildProviderTargets(ctx context.Context, matches []routing.ProviderMatch, gw *gateway.NenyaGateway, accountSelector routing.AccountSelector) []routing.UpstreamTarget {
	targets := make([]routing.UpstreamTarget, 0, len(matches))
	for _, m := range matches {
		provider, ok := gw.Providers[m.Provider]
		if !ok {
			continue
		}
		url := routing.ProviderURL(m.Provider, "", m.Format, provider.FormatURLs, gw.Providers)
		t := routing.UpstreamTarget{
			URL:        url,
			Model:      m.Model,
			Format:     m.Format,
			Provider:   m.Provider,
			MaxContext: m.MaxContext,
			MaxOutput:  m.MaxOutput,
		}
		if accountSelector != nil {
			if cred, acctID, ok := accountSelector.SelectCredentialForModel(ctx, m.Provider, m.Model); ok {
				t.AccountName = acctID
				t.Credential = cred
			}
		}
		targets = append(targets, t)
	}
	return targets
}

// resolveCache checks the response cache and returns a cache key. If a cached
// response is found and served, it returns ("", httpError) to signal early return.
func (p *Proxy) resolveCache(w http.ResponseWriter, r *http.Request, gw *gateway.NenyaGateway, req *chatRequest) (string, *httpError) {
	if gw.ResponseCache == nil {
		return "", nil
	}

	authToken := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	cacheKey := infra.FingerprintPayloadWithAuth(req.Payload, authToken)
	if r.Header.Get(gw.Config.ResponseCache.ForceRefreshHeader) == "" {
		embedFunc := p.buildEmbedFunc(gw, r, req.Payload)
		model := req.ModelName
		if data, ok, cacheType := gw.ResponseCache.Lookup(cacheKey, model, embedFunc); ok {
			p.replayCachedResponse(gw, w, r, data, cacheType, req.Stream)
			return "", &httpError{http.StatusNoContent, "cache hit"}
		}
	}
	return cacheKey, nil
}

func (p *Proxy) buildEmbedFunc(gw *gateway.NenyaGateway, r *http.Request, payload map[string]any) func() ([]float32, error) {
	if !gw.Config.ResponseCache.EnableSemantic {
		return nil
	}

	return func() ([]float32, error) {
		messagesRaw, ok := payload["messages"]
		if !ok {
			return nil, nil
		}
		messages, ok := messagesRaw.([]any)
		if !ok || len(messages) == 0 {
			return nil, nil
		}

		userText := p.extractUserMessagesForEmbedding(gw, messages)
		if userText == "" {
			return nil, nil
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		return gw.Embedder.Embed(ctx, userText)
	}
}

func (p *Proxy) extractUserMessagesForEmbedding(gw *gateway.NenyaGateway, messages []any) string {
	const maxEmbeddingTextLen = 10000

	var userMsgs strings.Builder
	userMsgs.Grow(min(maxEmbeddingTextLen, len(messages)*100))

	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			gw.Logger.Debug("skipping non-object message in semantic embedding")
			continue
		}
		role, ok := msg["role"].(string)
		if !ok || role != "user" {
			continue
		}
		content, ok := msg["content"].(string)
		if !ok {
			gw.Logger.Debug("skipping non-string content in semantic embedding", "role", role)
			continue
		}

		// Enforce size limit to prevent unbounded string growth
		if userMsgs.Len()+len(content) > maxEmbeddingTextLen {
			break
		}

		userMsgs.WriteString(content)
		userMsgs.WriteString("\n")
	}
	return userMsgs.String()
}

// resolvePipelineContext extracts messages, MCP tool state, limits, and client
// profile from the validated request, preparing the payload for the content
// filtering pipeline.
// Returns messages, MCP tools flag, soft/hard limits, window max context, and client profile.
// Proactive truncation thresholds:
//   - SoftLimit: triggers Ollama summarization (1/8 of MaxContext)
//   - HardLimit: absolute truncation limit (3/4 of MaxContext, leaves room for response)
//
// If MaxContext is unknown (<=0), truncation is disabled (limits=0) and the full payload
// is sent upstream. The upstream provider may return context_length_exceeded, which triggers
// automatic retry with summarization.
func (p *Proxy) resolvePipelineContext(r *http.Request, gw *gateway.NenyaGateway, req *chatRequest) ([]any, bool, int, int, int, pipeline.ClientProfile) {
	messagesRaw, ok := req.Payload["messages"]
	if !ok {
		return nil, false, 0, 0, 0, pipeline.ClientProfile{}
	}
	messages, ok := messagesRaw.([]any)
	if !ok || len(messages) == 0 {
		gw.Logger.Warn("messages field is not a non-empty array, skipping Ollama interception")
		return nil, false, 0, 0, 0, pipeline.ClientProfile{}
	}

	autoSearchCtx, autoSearchCancel := context.WithTimeout(r.Context(), mcpAutoSearchTimeout)
	p.injectAutoSearch(gw, autoSearchCtx, req.Payload, messages, req.AgentName)
	autoSearchCancel()
	p.injectMCPTools(gw, req.Payload, req.AgentName)

	softLimit := 0
	hardLimit := 0
	if len(req.Targets) > 0 {
		primaryTarget := req.Targets[0]
		if primaryTarget.MaxContext > 0 {
			softLimit = primaryTarget.MaxContext / 8
			hardLimit = primaryTarget.MaxContext * 3 / 4
		} else {
			gw.Logger.Warn("MaxContext unknown for model, proactive truncation disabled — configure max_context to enable",
				"model", req.ModelName,
				"provider", primaryTarget.Provider)
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
func (p *Proxy) handleChatCompletions(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
	gwStart := time.Now()
	keyRef := ""
	if apiKey != nil {
		keyRef = apiKey.Name
	}
	req, herr := p.validateChatRequest(w, r, gw, keyRef)
	if herr != nil {
		if herr.Code == http.StatusNoContent {
			return
		}
		writeStructuredError(w, herr.Code, infra.ErrorKindInvalidRequest, herr.Message)
		return
	}

	if err := p.applyContentPipeline(gw, r.Context(), req.Payload, req.TokenCount, req.WindowMaxCtx, req.Profile, req.SoftLimit, req.HardLimit); err != nil {
		gw.Logger.Warn("content pipeline failed, proceeding with original payload", "err", err)
	}
	gw.Metrics.RecordGatewayProcessing(r.Method, infra.NormalizeMetricPath(r.URL.Path), time.Since(gwStart))

	if req.HasMCPTools {
		p.forwardToUpstreamWithMCP(gw, w, r, forwardOptions{
			Targets:      req.Targets,
			Payload:      req.Payload,
			Stream:       req.Stream,
			Cooldown:     req.Cooldown,
			TokenCount:   req.TokenCount,
			AgentName:    req.AgentName,
			MaxRetries:   req.MaxRetries,
			CacheKey:     req.CacheKey,
			KeyRef:       req.KeyRef,
			SourceFormat: req.SourceFormat,
		})
		return
	}

	p.forwardToUpstream(gw, w, r, forwardOptions{
		Targets:      req.Targets,
		Payload:      req.Payload,
		Stream:       req.Stream,
		Cooldown:     req.Cooldown,
		TokenCount:   req.TokenCount,
		AgentName:    req.AgentName,
		MaxRetries:   req.MaxRetries,
		CacheKey:     req.CacheKey,
		KeyRef:       req.KeyRef,
		SourceFormat: req.SourceFormat,
	})
}

// replayCachedResponse serves a previously cached response, setting Content-Type
// based on whether the original request was streaming (SSE) or non-streaming (JSON).
func (p *Proxy) replayCachedResponse(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, data []byte, cacheType string, stream bool) {
	var cacheStatus string
	switch cacheType {
	case "exact":
		cacheStatus = "HIT"
	case "semantic":
		cacheStatus = "SEMI-HIT"
	default:
		cacheStatus = "HIT"
	}

	gw.Logger.Info("response cache hit", "type", cacheType)
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Nenya-Cache-Status", cacheStatus)
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
	if gw.InterceptorChain == nil {
		return nil
	}

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

	if !profile.IsIDE {
		if pipeline.PruneStaleToolCalls(payload, gw.Config.Compaction) {
			gw.Metrics.RecordCompaction()
		}
		if pipeline.PruneThoughts(payload, gw.Config.Compaction) {
			gw.Metrics.RecordCompaction()
		}
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

	msgObjs := make([]map[string]any, len(messages))
	for i, m := range messages {
		if obj, ok := m.(map[string]any); ok {
			msgObjs[i] = obj
		}
	}

	req := &pipeline.InterceptRequest{
		Payload:    payload,
		Messages:   msgObjs,
		Profile:    profile,
		SoftLimit:  softLimit,
		HardLimit:  hardLimit,
		TokenCount: tokenCount,
	}

	_, err := gw.InterceptorChain.Execute(ctx, req)
	return err
}

// buildWindowDeps creates a WindowDeps from the gateway state.
func buildWindowDeps(gw *gateway.NenyaGateway) pipeline.WindowDeps {
	return pipeline.WindowDeps{
		Logger:       gw.Logger,
		Client:       gw.Client,
		OllamaClient: gw.OllamaClient,
		Providers:    gw.Providers,
		InjectAPIKey: func(providerName string, headers http.Header) error {
			return routing.InjectAPIKeyWithGateway(providerName, gw, headers)
		},
		CountTokens: gw.CountTokens,
	}
}

func (p *Proxy) buildUpstreamRequest(gw *gateway.NenyaGateway, ctx context.Context, method, url string, body []byte, providerName, modelName, preselectedCredential string, srcHeaders http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create upstream request: %w", err)
	}
	if err := routing.InjectAPIKeyWithGatewayCtx(ctx, providerName, modelName, gw, req.Header, preselectedCredential); err != nil {
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

func (p *Proxy) handleNonStreamingResponse(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, target routing.UpstreamTarget, agentName, sourceFormat string, action upstreamAction, cacheKey string, cooldownDuration time.Duration) streamResult {
	defer action.cancel()

	const maxNonStreamingResponseBytes = 10 * 1024 * 1024
	respBody, err := io.ReadAll(io.LimitReader(action.resp.Body, maxNonStreamingResponseBytes))
	if err != nil {
		gw.Logger.Error("failed to read non-streaming response body", "err", err)
		writeGatewayError(w, http.StatusBadGateway, ErrorTypeProvider, "Failed to read upstream response")
		return streamResult{empty: true}
	}
	_ = action.resp.Body.Close()

	if len(respBody) == 0 {
		gw.AgentState.RecordFailure(target, cooldownDuration)
		gw.Logger.Warn("empty non-streaming response from upstream", "model", target.Model)
		return streamResult{empty: true}
	}
	if len(respBody) >= maxNonStreamingResponseBytes {
		gw.Logger.Error("non-streaming response exceeded size limit", "model", target.Model)
		writeGatewayError(w, http.StatusBadGateway, ErrorTypeProvider, "Upstream response too large")
		return streamResult{empty: true}
	}

	var responseMap map[string]interface{}
	if err := json.Unmarshal(respBody, &responseMap); err != nil {
		gw.Logger.Error("failed to parse non-streaming JSON response", "err", err, "body", string(respBody))
		writeGatewayError(w, http.StatusBadGateway, ErrorTypeProvider, "Invalid JSON response from upstream")
		return streamResult{}
	}

	if sourceFormat == "anthropic" && target.Format != "anthropic" {
		a := adapter.GetAnthropicAdapter()
		responseMap = a.ConvertOpenAIResponseToAnthropicBody(responseMap)
	} else if target.Format == "anthropic" {
		a := adapter.GetAnthropicAdapter()
		responseMap = a.ConvertAnthropicToOpenAIBody(responseMap, false)
	}

	if usage, ok := responseMap["usage"].(map[string]interface{}); ok {
		recordNonStreamingUsage(r.Context(), gw, target, agentName, usage)
	}

	gw.ExtractQuotaFromResponseHeaders(r.Context(), target.Provider, target.AccountName, action.resp.Header)

	routing.CopyHeaders(action.resp.Header, w.Header())
	w.WriteHeader(action.resp.StatusCode)
	if err := json.NewEncoder(w).Encode(responseMap); err != nil {
		gw.Logger.Debug("non-streaming response write failed", "err", err)
	}
	return streamResult{}
}
