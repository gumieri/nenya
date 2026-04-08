package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maxOllamaResponseBytes = 512 * 1024

const maxModelNameLength = 256

const maxErrorBodyBytes = 8 * 1024

var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"content-length":      true,
	"content-encoding":    true,
	"upgrade":             true,
	"transfer-encoding":   true,
	"te":                  true,
	"trailers":            true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"keep-alive":          true,
	"proxy-connection":    true,
}

func (g *NenyaGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			g.logger.Error("panic recovered", "err", rec, "stack", string(debug.Stack()))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}()

	switch {
	case r.URL.Path == "/healthz":
		g.observeHTTP(g.handleHealthz)(w, r)
		return
	case r.URL.Path == "/statsz":
		g.observeHTTP(g.handleStats)(w, r)
		return
	case r.URL.Path == "/metrics":
		if !g.authenticateRequest(r, w) {
			return
		}
		g.observeHTTPFunc(g.handleMetrics)(w, r)
		return
	case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
		if !g.authenticateRequest(r, w) {
			return
		}
		g.observeHTTP(g.handleModels)(w, r)
		return
	case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
		if !g.authenticateRequest(r, w) {
			return
		}
		g.observeHTTPFunc(g.handleChatCompletions)(w, r)
		return
	case r.URL.Path == "/v1/embeddings" && r.Method == http.MethodPost:
		if !g.authenticateRequest(r, w) {
			return
		}
		g.observeHTTPFunc(g.handleEmbeddings)(w, r)
		return
	default:
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
}

func (g *NenyaGateway) authenticateRequest(r *http.Request, w http.ResponseWriter) bool {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		g.logger.Warn("missing or malformed Authorization header")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}
	clientToken := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if subtle.ConstantTimeCompare([]byte(clientToken), []byte(g.secrets.ClientToken)) != 1 {
		g.logger.Warn("invalid client token")
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func (g *NenyaGateway) handleModels(w http.ResponseWriter) {
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}

	var models []modelEntry
	seen := make(map[string]bool)

	addModel := func(id, ownedBy string) {
		if seen[id] {
			return
		}
		seen[id] = true
		models = append(models, modelEntry{
			ID:      id,
			Object:  "model",
			OwnedBy: ownedBy,
		})
	}

	for agentName, agent := range g.config.Agents {
		addModel(agentName, "nenya")
		for _, m := range agent.Models {
			addModel(m.Model, m.Provider)
		}
	}

	for _, p := range g.providers {
		if p.APIKey == "" && p.AuthStyle != "none" {
			continue
		}
		for _, prefix := range p.RoutePrefixes {
			addModel(prefix+"*", p.Name)
		}
	}

	resp := map[string]interface{}{
		"object": "list",
		"data":   models,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		g.logger.Error("failed to encode models response", "err", err)
	}
}

func (g *NenyaGateway) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, g.config.Server.MaxBodyBytes)
	defer r.Body.Close()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		g.logger.Error("failed to read embeddings request body", "err", err)
		http.Error(w, "Payload too large or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	var payload map[string]interface{}
	if err = json.Unmarshal(bodyBytes, &payload); err != nil {
		g.logger.Warn("failed to parse embeddings JSON")
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	modelName, ok := payload["model"].(string)
	if !ok || modelName == "" {
		g.logger.Warn("missing or empty model in embeddings request")
		http.Error(w, `Missing or empty "model" field`, http.StatusBadRequest)
		return
	}
	if len(modelName) > maxModelNameLength {
		g.logger.Warn("model name exceeds maximum length in embeddings request", "length", len(modelName))
		http.Error(w, "Model name too long", http.StatusBadRequest)
		return
	}

	provider := g.resolveProvider(modelName)
	if provider == nil {
		g.logger.Warn("no provider for embeddings model", "model", modelName)
		http.Error(w, "No provider configured for this model", http.StatusBadRequest)
		return
	}

	embeddingURL := strings.TrimSuffix(provider.URL, "/chat/completions") + "/embeddings"
	if embeddingURL == provider.URL {
		g.logger.Warn("provider URL does not end with /chat/completions, cannot derive embeddings endpoint",
			"provider", provider.Name, "url", provider.URL)
		http.Error(w, "Provider does not support embeddings", http.StatusBadRequest)
		return
	}

	req, err := g.buildUpstreamRequest(r.Context(), http.MethodPost, embeddingURL, bodyBytes, provider.Name, r.Header)
	if err != nil {
		g.logger.Error("failed to create embeddings upstream request", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		g.logger.Error("embeddings upstream request failed", "provider", provider.Name, "err", err)
		http.Error(w, "Upstream provider error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeaders(resp.Header, w.Header())
	w.WriteHeader(resp.StatusCode)

	if _, err := io.Copy(w, resp.Body); err != nil {
		g.logger.Debug("embeddings response copy ended", "err", err)
	}
}

func (g *NenyaGateway) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, g.config.Server.MaxBodyBytes)
	defer r.Body.Close()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		g.logger.Error("failed to read request body", "err", err)
		http.Error(w, "Payload too large or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	if r.Context().Err() != nil {
		return
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		g.logger.Warn("failed to parse JSON, returning Bad Request")
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	modelName, ok := payload["model"].(string)
	if !ok || modelName == "" {
		g.logger.Warn("missing or empty model field in request body")
		http.Error(w, `Missing or empty "model" field in request body`, http.StatusBadRequest)
		return
	}
	if len(modelName) > maxModelNameLength {
		g.logger.Warn("model name exceeds maximum length", "length", len(modelName))
		http.Error(w, "Model name too long", http.StatusBadRequest)
		return
	}

	tokenCount := g.countRequestTokens(payload)

	var targets []upstreamTarget
	var cooldownDuration time.Duration
	var agentName string

	if agent, ok := g.config.Agents[modelName]; ok {
		agentName = modelName
		secs := agent.CooldownSeconds
		if secs == 0 {
			secs = defaultAgentCooldownSec
		}
		cooldownDuration = time.Duration(secs) * time.Second
		targets = g.buildTargetList(modelName, agent, tokenCount)
		if len(targets) == 0 {
			if len(agent.Models) > 0 {
				g.logger.Warn("all models excluded by max_context",
					"agent", modelName, "tokens", tokenCount)
				http.Error(w, "Request too large for all configured models in this agent", http.StatusRequestEntityTooLarge)
			} else {
				g.logger.Error("agent has no models configured", "agent", modelName)
				http.Error(w, "Agent has no models configured", http.StatusInternalServerError)
			}
			return
		}
		strategy := agent.Strategy
		if strategy == "" {
			strategy = "round-robin"
		}
		g.logger.Info("agent routing",
			"agent", modelName, "strategy", strategy, "models_in_chain", len(targets))
	} else {
		var upstreamURL string
		var providerName string
		if p := g.resolveProvider(modelName); p != nil {
			upstreamURL = p.URL
			providerName = p.Name
		} else {
			g.logger.Warn("no provider found for model", "model", modelName)
			http.Error(w, "No provider configured for this model", http.StatusBadRequest)
			return
		}
		targets = []upstreamTarget{{url: upstreamURL, model: modelName, provider: providerName}}
		g.logger.Info("model routing", "model", modelName, "upstream", upstreamURL)
	}

	if messagesRaw, ok := payload["messages"]; ok {
		if messages, ok := messagesRaw.([]interface{}); ok && len(messages) > 0 {
			windowMaxCtx := g.resolveWindowMaxContext(modelName, targets)
			if err := g.applyContentPipeline(r.Context(), payload, tokenCount, windowMaxCtx); err != nil {
				g.logger.Error("content pipeline failed", "err", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
		} else {
			g.logger.Warn("messages field is not a non-empty array, skipping Ollama interception")
		}
	}

	g.forwardToUpstream(w, r, targets, payload, cooldownDuration, tokenCount, agentName)
}

func (g *NenyaGateway) applyContentPipeline(ctx context.Context, payload map[string]interface{}, tokenCount int, windowMaxCtx int) error {
	messages, ok := payload["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		return nil
	}

	g.applyPrefixCacheOptimizations(payload, messages)

	anyRedacted := false
	for _, msgRaw := range messages {
		msgNode, isMap := msgRaw.(map[string]interface{})
		if !isMap {
			continue
		}
		if g.shouldSkipRedaction(msgNode) {
			continue
		}
		text := extractContentText(msgNode)
		if text == "" {
			continue
		}
		if redacted := g.redactSecrets(text); redacted != text {
			msgNode["content"] = redacted
			anyRedacted = true
		}
	}
	if anyRedacted {
		if g.metrics != nil {
			g.metrics.RecordRedaction()
		}
	}

	messages = payload["messages"].([]interface{})
	if len(messages) == 0 {
		return nil
	}
	if g.applyCompaction(messages) {
		if g.metrics != nil {
			g.metrics.RecordCompaction()
		}
	}

	messages = payload["messages"].([]interface{})
	if len(messages) == 0 {
		return nil
	}
	if windowed, err := g.applyWindowCompaction(ctx, payload, messages, tokenCount, windowMaxCtx); err != nil {
		g.logger.Warn("window compaction failed, proceeding without it", "err", err)
	} else if windowed {
		if g.metrics != nil {
			g.metrics.RecordWindow(g.config.Window.Mode)
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

	textForInterception := extractContentText(lastMsgNode)
	if textForInterception == "" {
		g.logger.Warn("last message has no text content, skipping interception")
		return nil
	}

	contentRunes := utf8.RuneCountInString(textForInterception)
	softLimit := g.config.Governance.ContextSoftLimit
	hardLimit := g.config.Governance.ContextHardLimit

	var processed string
	var needsUpdate bool

	if contentRunes < softLimit {
		g.logger.Debug("payload within soft limit, passing through",
			"runes", contentRunes, "soft_limit", softLimit)
	} else if contentRunes <= hardLimit {
		g.logger.Warn("payload exceeds soft limit, sending to Ollama",
			"runes", contentRunes)
		if g.metrics != nil {
			g.metrics.RecordInterception("soft_limit")
		}
		summarized, err := g.summarizeWithOllama(ctx, textForInterception)
		if err != nil {
			g.logger.Error("Ollama summarization failed, proceeding with original", "err", err)
		} else {
			processed = fmt.Sprintf("[Nenya Sanitized via Ollama]:\n%s", summarized)
			needsUpdate = true
		}
	} else {
		g.logger.Warn("payload exceeds hard limit, truncating before Ollama",
			"runes", contentRunes, "hard_limit", hardLimit)
		if g.metrics != nil {
			g.metrics.RecordInterception("hard_limit")
		}
		truncated := g.truncateMiddleOut(textForInterception, hardLimit)
		summarized, err := g.summarizeWithOllama(ctx, truncated)
		if err != nil {
			g.logger.Error("Ollama summarization failed after truncation, forwarding truncated", "err", err)
			processed = fmt.Sprintf("[Nenya Truncated (Ollama unreachable)]:\n%s", truncated)
		} else {
			processed = fmt.Sprintf("[Nenya Sanitized via Ollama (truncated input)]:\n%s", summarized)
		}
		needsUpdate = true
	}

	if needsUpdate {
		lastMsgNode["content"] = processed
	}

	return nil
}

func (g *NenyaGateway) summarizeWithOllama(ctx context.Context, heavyText string) (string, error) {
	engine := g.config.SecurityFilter.Engine
	ctx, cancel := context.WithTimeout(ctx, time.Duration(engine.TimeoutSeconds)*time.Second)
	defer cancel()

	defaultPrompt := "You are a data privacy filter. Review the following text and remove or replace any IP addresses, AWS keys (AKIA...), passwords, tokens, or credentials with [REDACTED]. Preserve the original structure, detail level, and all non-sensitive content exactly as provided. Do NOT summarize or shorten the text."
	systemPrompt, err := loadPromptFile(engine.SystemPromptFile, engine.SystemPrompt, defaultPrompt)
	if err != nil {
		g.logger.Warn("failed to load privacy filter prompt, using default", "err", err)
		systemPrompt = defaultPrompt
	}

	summary, err := g.callEngine(ctx, engine, systemPrompt, heavyText)
	if err != nil {
		return "", err
	}
	return summary, nil
}

func (g *NenyaGateway) forwardToUpstream(
	w http.ResponseWriter,
	r *http.Request,
	targets []upstreamTarget,
	payload map[string]interface{},
	cooldownDuration time.Duration,
	tokenCount int,
	agentName string,
) {
	originalPayload, err := json.Marshal(payload)
	if err != nil {
		g.logger.Error("failed to marshal original payload for retry loop", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if g.config.Compaction.Enabled && g.config.Compaction.JSONMinify {
		minified := bytes.NewBuffer(make([]byte, 0, len(originalPayload)))
		err = json.Compact(minified, originalPayload)
		if err != nil {
			g.logger.Warn("failed to minify JSON payload, using original", "err", err)
		} else {
			originalPayload = minified.Bytes()
		}
	}

	for i, target := range targets {
		workingPayload := make(map[string]interface{})
		if err := json.Unmarshal(originalPayload, &workingPayload); err != nil {
			g.logger.Error("failed to unmarshal payload for target",
				"target", i+1, "total", len(targets), "err", err)
			continue
		}

		action := g.prepareAndSend(r, i, targets, target, workingPayload, cooldownDuration, tokenCount, agentName)
		switch action.kind {
		case actionContinue:
			continue
		case actionError:
			action.body, _ = io.ReadAll(io.LimitReader(action.resp.Body, maxErrorBodyBytes))
			action.resp.Body.Close()
			shouldContinue := g.handleUpstreamError(r, i, targets, target, cooldownDuration, agentName, action)
			action.cancel()
			if shouldContinue {
				continue
			}
			http.Error(w, "Upstream provider error", action.resp.StatusCode)
			return
		case actionStream:
			g.streamResponse(w, r, target, agentName, action)
			return
		}
	}

	g.logger.Error("all upstream targets exhausted", "total", len(targets))
	if g.metrics != nil && agentName != "" {
		g.metrics.RecordExhausted(agentName)
	}
	http.Error(w, "All upstream targets exhausted", http.StatusServiceUnavailable)
}

type upstreamAction struct {
	kind   int
	resp   *http.Response
	body   []byte
	cancel context.CancelFunc
}

const (
	actionContinue = iota
	actionError
	actionStream
)

func (g *NenyaGateway) prepareAndSend(
	r *http.Request,
	idx int,
	targets []upstreamTarget,
	target upstreamTarget,
	payload map[string]interface{},
	cooldownDuration time.Duration,
	tokenCount int,
	agentName string,
) upstreamAction {
	g.stats.RecordRequest(target.model, tokenCount)
	if g.metrics != nil {
		g.metrics.RecordTokens("input", target.model, agentName, target.provider, tokenCount)
		g.metrics.RecordUpstreamRequest(target.model, agentName, target.provider)
	}

	if !g.checkRateLimit(target.url, tokenCount) {
		if g.metrics != nil {
			g.metrics.RecordRateLimitRejected(extractHost(target.url))
		}
		g.logger.Warn("target skipped: rate limit exceeded",
			"target", idx+1, "total", len(targets), "model", target.model)
		return upstreamAction{kind: actionContinue}
	}

	transformedBody, finalModel, err := g.transformRequestForUpstream(target.provider, target.url, payload, target.model, target.maxOutput)
	if err != nil {
		g.logger.Warn("failed to transform request, using original payload",
			"target", idx+1, "total", len(targets), "model", target.model, "err", err)
		transformedBody, _ = json.Marshal(payload)
	} else if finalModel != "" {
		g.logger.Debug("using model for target",
			"target", idx+1, "total", len(targets), "model", finalModel, "url", target.url)
	}

	req, err := g.buildUpstreamRequest(r.Context(), r.Method, target.url, transformedBody, target.provider, r.Header)
	if err != nil {
		g.logger.Error("failed to create upstream request",
			"target", idx+1, "total", len(targets), "err", err)
		return upstreamAction{kind: actionContinue}
	}

	if g.logger.Enabled(r.Context(), slog.LevelDebug) {
		debugHeaders := make(http.Header)
		for k, v := range req.Header {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "key") || strings.Contains(lk, "auth") {
				debugHeaders[k] = []string{"[REDACTED]"}
			} else {
				debugHeaders[k] = v
			}
		}
		g.logger.Debug("forwarding to upstream",
			"url", target.url, "target", idx+1, "total", len(targets))
		g.logger.Debug("request headers", "headers", debugHeaders)
		if len(transformedBody) > 0 && len(transformedBody) < 1000 {
			g.logger.Debug("request body", "body", string(transformedBody))
		}
	}

	upstreamCtx, upstreamCancel := context.WithCancel(r.Context())
	req = req.WithContext(upstreamCtx)

	resp, err := g.client.Do(req)
	if err != nil {
		upstreamCancel()
		g.logger.Warn("target network error",
			"target", idx+1, "total", len(targets), "model", target.model, "err", err)
		return upstreamAction{kind: actionContinue}
	}

	g.logger.Info("upstream response",
		"target", idx+1, "total", len(targets), "model", target.model, "status", resp.StatusCode)

	if resp.StatusCode >= 400 {
		g.stats.RecordError(target.model)
		if g.metrics != nil {
			g.metrics.RecordUpstreamError(target.model, agentName, target.provider, resp.StatusCode)
		}
		return upstreamAction{kind: actionError, resp: resp, cancel: upstreamCancel}
	}

	return upstreamAction{kind: actionStream, resp: resp, cancel: upstreamCancel}
}

func parseQuotaExhaustion(body []byte) time.Duration {
	if len(body) == 0 {
		return 0
	}
	lower := strings.ToLower(string(body))

	if idx := strings.Index(lower, "per 86400s"); idx != -1 {
		return maxQuotaCooldown
	}
	if idx := strings.Index(lower, "perday"); idx != -1 {
		return maxQuotaCooldown
	}

	quotaPatterns := []string{
		"resource_exhausted",
		"quota exceeded",
		"quota_exceeded",
	}
	for _, p := range quotaPatterns {
		if strings.Contains(lower, p) {
			return 5 * time.Minute
		}
	}

	return 0
}

func (g *NenyaGateway) handleUpstreamError(
	r *http.Request,
	idx int,
	targets []upstreamTarget,
	target upstreamTarget,
	cooldownDuration time.Duration,
	agentName string,
	action upstreamAction,
) bool {
	errorBody := action.body

	if g.isRetryableStatus(target.provider, action.resp.StatusCode) {
		if len(errorBody) > 0 {
			g.logger.Warn("upstream retryable error",
				"target", idx+1, "total", len(targets), "model", target.model,
				"status", action.resp.StatusCode, "body", string(errorBody))
		} else {
			g.logger.Warn("activating cooldown, trying next target",
				"target", idx+1, "total", len(targets), "model", target.model, "status", action.resp.StatusCode)
		}
		effectiveCooldown := cooldownDuration
		if action.resp.StatusCode == http.StatusTooManyRequests && len(errorBody) > 0 {
			if quotaCD := parseQuotaExhaustion(errorBody); quotaCD > 0 {
				if quotaCD > cooldownDuration {
					effectiveCooldown = quotaCD
					g.logger.Info("quota exhaustion detected, extending cooldown",
						"model", target.model, "cooldown_s", effectiveCooldown.Seconds())
				}
			}
		}
		g.activateCooldown(target, effectiveCooldown, agentName)
		if action.resp.StatusCode == http.StatusTooManyRequests {
			delay := parseRetryDelay(action.resp.Header, errorBody)
			if delay > 0 {
				g.logger.Info("rate limited, backing off before retry",
					"model", target.model, "delay_ms", delay.Milliseconds())
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-r.Context().Done():
					timer.Stop()
				}
			}
		}
		return true
	}

	if isRetryableClientError(action.resp.StatusCode, errorBody) && len(targets) > 1 {
		g.logger.Warn("retryable client error from upstream, trying next target",
			"target", idx+1, "total", len(targets), "model", target.model,
			"status", action.resp.StatusCode, "body", string(errorBody))
		g.activateCooldown(target, cooldownDuration, agentName)
		return true
	}

	defer action.cancel()
	if len(errorBody) > 0 {
		g.logger.Error("non-retryable upstream error",
			"target", idx+1, "total", len(targets), "model", target.model,
			"status", action.resp.StatusCode, "body", string(errorBody))
	} else {
		g.logger.Error("non-retryable upstream error, empty body",
			"target", idx+1, "total", len(targets), "model", target.model, "status", action.resp.StatusCode)
	}
	return false
}

func (g *NenyaGateway) activateCooldown(target upstreamTarget, cooldownDuration time.Duration, agentName string) {
	if target.coolKey == "" || cooldownDuration == 0 {
		return
	}
	if g.metrics != nil {
		g.metrics.RecordCooldown(agentName, target.provider, target.model)
	}
	g.agentMu.Lock()
	g.modelCooldowns[target.coolKey] = time.Now().Add(cooldownDuration)
	g.agentMu.Unlock()
}

const streamIdleTimeout = 120 * time.Second

type stallReader struct {
	src     io.Reader
	mu      sync.Mutex
	timer   *time.Timer
	stalled bool
}

func newStallReader(src io.Reader, timeout time.Duration) *stallReader {
	sr := &stallReader{src: src}
	sr.timer = time.AfterFunc(timeout, func() {
		sr.mu.Lock()
		sr.stalled = true
		sr.mu.Unlock()
	})
	return sr
}

func (sr *stallReader) Read(p []byte) (int, error) {
	sr.mu.Lock()
	if sr.stalled {
		sr.mu.Unlock()
		return 0, errStreamStalled
	}
	sr.mu.Unlock()

	n, err := sr.src.Read(p)
	if n > 0 {
		sr.timer.Reset(streamIdleTimeout)
	}
	return n, err
}

func (sr *stallReader) Stop() {
	sr.timer.Stop()
}

var errStreamStalled = errors.New("stream stalled: no data received within idle timeout")

func (g *NenyaGateway) streamResponse(
	w http.ResponseWriter,
	r *http.Request,
	target upstreamTarget,
	agentName string,
	action upstreamAction,
) {
	defer action.cancel()
	copyHeaders(action.resp.Header, w.Header())
	w.WriteHeader(action.resp.StatusCode)

	transformer := g.getResponseTransformer(target.provider)
	if transformer != nil {
		g.logger.Debug("SSE transformer active", "provider", target.provider)
	}

	stallR := newStallReader(action.resp.Body, streamIdleTimeout)
	defer stallR.Stop()

	transformingReader := NewSSETransformingReader(stallR, transformer)
	transformingReader.SetOnUsage(func(completion, prompt, total int) {
		g.stats.RecordOutput(target.model, completion)
		if g.metrics != nil {
			g.metrics.RecordTokens("output", target.model, agentName, target.provider, completion)
		}
	})

	if g.config.SecurityFilter.OutputEnabled && (len(g.secretPatterns) > 0 || len(g.blockedPatterns) > 0) {
		sf := NewStreamFilter(g.secretPatterns, g.blockedPatterns, g.config.SecurityFilter.RedactionLabel, g.config.SecurityFilter.OutputWindowChars)
		transformingReader.SetStreamFilter(sf)
		g.logger.Debug("stream filter active",
			"secret_patterns", len(g.secretPatterns),
			"block_patterns", len(g.blockedPatterns),
			"window_size", g.config.SecurityFilter.OutputWindowChars)
	}

	var copyErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, copyErr = io.Copy(w, transformingReader)
	}()

	select {
	case <-done:
		if errors.Is(copyErr, ErrStreamBlocked) {
			action.cancel()
			action.resp.Body.Close()
			g.logger.Warn("stream blocked by execution policy, upstream killed",
				"model", target.model, "provider", target.provider)
			if g.metrics != nil {
				g.metrics.RecordStreamBlock(target.model, target.provider)
			}
			g.writeBlockedSSE(w)
			return
		}
		if errors.Is(copyErr, errStreamStalled) {
			action.cancel()
			action.resp.Body.Close()
			g.logger.Warn("stream stalled, aborting upstream",
				"model", target.model, "provider", target.provider,
				"idle_timeout", streamIdleTimeout)
			return
		}
		action.resp.Body.Close()
	case <-r.Context().Done():
		g.logger.Info("client disconnected, aborting upstream stream", "model", target.model)
		action.resp.Body.Close()
		<-done
	}
}

func (g *NenyaGateway) buildUpstreamRequest(ctx context.Context, method, url string, body []byte, providerName string, srcHeaders http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create upstream request: %w", err)
	}
	headers := srcHeaders.Clone()
	headers.Del("Authorization")
	if err := g.injectAPIKey(providerName, headers); err != nil {
		return nil, fmt.Errorf("API key injection failed: %w", err)
	}
	copyHeaders(headers, req.Header)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", g.config.Server.UserAgent)
	return req, nil
}

const maxRetryBackoff = 5 * time.Second
const maxQuotaCooldown = 30 * time.Minute

type rpcDetail struct {
	RetryDelay string `json:"retryDelay"`
	Type       string `json:"@type"`
}

func capRetryDelay(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if d > maxRetryBackoff {
		return maxRetryBackoff
	}
	return d
}

func parseRetryDelayFromRPCDetails(details []rpcDetail) time.Duration {
	for _, d := range details {
		if d.RetryDelay != "" {
			if dur, err := time.ParseDuration(d.RetryDelay); err == nil {
				return capRetryDelay(dur)
			}
		}
	}
	return 0
}

func parseRetryDelayFromMessage(msg string) time.Duration {
	lower := strings.ToLower(msg)

	patterns := []struct {
		before string
		after  string
	}{
		{"retry in ", "s"},
		{"wait ", "s"},
		{"retry after ", "s"},
	}
	for _, p := range patterns {
		idx := strings.Index(lower, p.before)
		if idx == -1 {
			continue
		}
		candidate := lower[idx+len(p.before):]
		end := len(candidate)
		for i, c := range candidate {
			if c < '0' || c > '9' {
				end = i
				break
			}
		}
		if end == 0 {
			continue
		}
		n, err := strconv.ParseFloat(candidate[:end], 64)
		if err != nil || n <= 0 {
			continue
		}
		return capRetryDelay(time.Duration(n * float64(time.Second)))
	}

	return 0
}

func parseRetryDelay(header http.Header, body []byte) time.Duration {
	if v := header.Get("Retry-After"); v != "" {
		if secs, err := time.ParseDuration(v + "s"); err == nil && secs > 0 {
			return capRetryDelay(secs)
		}
	}

	if len(body) == 0 {
		return 0
	}

	var envelope struct {
		Error struct {
			Details []rpcDetail `json:"details"`
			Message string      `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		if d := parseRetryDelayFromRPCDetails(envelope.Error.Details); d > 0 {
			return d
		}
		if envelope.Error.Message != "" {
			if d := parseRetryDelayFromMessage(envelope.Error.Message); d > 0 {
				return d
			}
		}
	}

	var arr []struct {
		Error struct {
			Details []rpcDetail `json:"details"`
			Message string      `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &arr); err == nil && len(arr) > 0 {
		if d := parseRetryDelayFromRPCDetails(arr[0].Error.Details); d > 0 {
			return d
		}
		if arr[0].Error.Message != "" {
			if d := parseRetryDelayFromMessage(arr[0].Error.Message); d > 0 {
				return d
			}
		}
	}

	return 0
}

var defaultRetryableStatusCodes = []int{
	http.StatusTooManyRequests,
	http.StatusInternalServerError,
	http.StatusBadGateway,
	http.StatusServiceUnavailable,
	http.StatusGatewayTimeout,
}

func (g *NenyaGateway) isRetryableStatus(providerName string, statusCode int) bool {
	if p, ok := g.providers[providerName]; ok && len(p.RetryableStatusCodes) > 0 {
		return sliceContains(p.RetryableStatusCodes, statusCode)
	}
	if len(g.config.Governance.RetryableStatusCodes) > 0 {
		return sliceContains(g.config.Governance.RetryableStatusCodes, statusCode)
	}
	return sliceContains(defaultRetryableStatusCodes, statusCode)
}

func sliceContains(haystack []int, needle int) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

func isRetryableClientError(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest && statusCode != http.StatusRequestEntityTooLarge {
		return false
	}
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "unavailable_model") ||
		strings.Contains(lower, "tokens_limit_reached") ||
		strings.Contains(lower, "context_length_exceeded") ||
		strings.Contains(lower, "model_overloaded") ||
		strings.Contains(lower, "overloaded") ||
		strings.Contains(lower, "thought_signature") ||
		strings.Contains(lower, "name cannot be empty") ||
		strings.Contains(lower, "messages parameter is illegal")
}

func copyHeaders(src, dst http.Header) {
	for k, vv := range src {
		if hopByHopHeaders[strings.ToLower(k)] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func (g *NenyaGateway) handleStats(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(g.stats.Snapshot()); err != nil {
		g.logger.Error("failed to encode stats response", "err", err)
	}
}

func (g *NenyaGateway) handleHealthz(w http.ResponseWriter) {
	engineOK := g.checkSecurityFilterEngineHealth()

	resp := map[string]interface{}{
		"status": "ok",
		"engine": map[string]interface{}{
			"status": engineOK,
		},
	}

	status := http.StatusOK
	if !engineOK {
		status = http.StatusServiceUnavailable
		resp["status"] = "degraded"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		g.logger.Error("failed to encode healthz response", "err", err)
	}
}

func (g *NenyaGateway) writeBlockedSSE(w http.ResponseWriter) {
	blockPayload := map[string]interface{}{
		"id":     "blocked",
		"object": "chat.completion.chunk",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"content": "[Response blocked by execution policy]",
				},
				"finish_reason": "stop",
			},
		},
	}
	blockJSON, err := json.Marshal(blockPayload)
	if err != nil {
		g.logger.Error("failed to marshal blocked SSE payload", "err", err)
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", blockJSON)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (g *NenyaGateway) checkSecurityFilterEngineHealth() bool {
	engine := g.config.SecurityFilter.Engine
	p, ok := g.providers[engine.Provider]
	if !ok {
		g.logger.Warn("engine provider not found", "provider", engine.Provider)
		return false
	}
	apiFormat := p.ApiFormat
	if apiFormat == "" {
		apiFormat = "openai"
	}
	if apiFormat != "ollama" {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ollamaHealthURL(p.URL), nil)
	if err != nil {
		return false
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func ollamaHealthURL(engineURL string) string {
	const nativeSuffix = "/api/generate"
	const openaiSuffix = "/v1/chat/completions"
	if strings.HasSuffix(engineURL, nativeSuffix) {
		return engineURL[:len(engineURL)-len(nativeSuffix)] + "/api/tags"
	}
	if strings.HasSuffix(engineURL, openaiSuffix) {
		return engineURL[:len(engineURL)-len(openaiSuffix)] + "/api/tags"
	}
	return engineURL
}
