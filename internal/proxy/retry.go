package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nenya/internal/adapter"
	"nenya/internal/gateway"
	"nenya/internal/infra"
	"nenya/internal/pipeline"
	"nenya/internal/routing"
)

const maxRetryBackoff = 5 * time.Second
const maxQuotaCooldown = 30 * time.Minute

const (
	exponentialBackoffBase   = 500 * time.Millisecond
	exponentialBackoffMax    = 8 * time.Second
	exponentialBackoffJitter = 750 * time.Millisecond
)

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
	actionResponse
)

func calculateBackoff(attempt int) time.Duration {
	delay := exponentialBackoffBase
	for i := 0; i < attempt; i++ {
		delay *= 2
		if delay >= exponentialBackoffMax {
			delay = exponentialBackoffMax
			break
		}
	}
	jitter := time.Duration(rand.Int63n(int64(exponentialBackoffJitter)))
	return delay + jitter
}

func waitWithCancel(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// forwardOptions holds the parameters for forwarding a request upstream.
type forwardOptions struct {
	Targets    []routing.UpstreamTarget
	Payload    map[string]any
	Stream     bool
	Cooldown   time.Duration
	TokenCount int
	AgentName  string
	MaxRetries int
	CacheKey   string
	KeyRef     string
}

// retryLoop encapsulates the state and logic for retrying upstream requests.
type retryLoop struct {
	p               *Proxy
	gw              *gateway.NenyaGateway
	w               http.ResponseWriter
	r               *http.Request
	opts            forwardOptions
	ctxLogger       *slog.Logger
	originalPayload []byte
	attempt         int
	stream          bool
}

// trackInFlight increments the in-flight gauge for the first target in
// the agent's fallback chain and returns a function that decrements it.
// The gauge uses the first target's model/agent/provider labels to represent
// the entire agent group's in-flight status. Safe to defer.
func (rl *retryLoop) trackInFlight() func() {
	if len(rl.opts.Targets) == 0 || rl.gw.Metrics == nil {
		return func() {}
	}
	rl.gw.Metrics.IncInFlight(rl.opts.Targets[0].Model, rl.opts.AgentName, rl.opts.Targets[0].Provider)
	return func() {
		rl.gw.Metrics.DecInFlight(rl.opts.Targets[0].Model, rl.opts.AgentName, rl.opts.Targets[0].Provider)
	}
}

// copyPayload clears and unmarshals the original payload into dest. Returns
// false on unmarshal failure (logs and skips target).
func (rl *retryLoop) copyPayload(dest map[string]any, idx int) bool {
	for k := range dest {
		delete(dest, k)
	}
	if err := json.Unmarshal(rl.originalPayload, &dest); err != nil {
		rl.ctxLogger.Error("failed to unmarshal payload for target",
			"target", idx+1, "total", len(rl.opts.Targets), "err", err)
		return false
	}
	return true
}

// handleActionResult dispatches on action.kind and returns true when the
// request is considered handled (success or terminal failure), false when
// the loop should try the next target.
func (rl *retryLoop) handleActionResult(i int, target routing.UpstreamTarget, action upstreamAction) bool {
	switch action.kind {
	case actionContinue:
		return false
	case actionError:
		switch rl.handleActionError(i, target, action) {
		case retrySignalDone:
			return true
		case retrySignalBreak:
			return false
		}
		return false
	case actionStream:
		result := rl.p.streamResponse(rl.gw, rl.w, rl.r, target, rl.opts.AgentName, action, rl.opts.CacheKey, rl.opts.Cooldown)
		if result.empty {
			rl.ctxLogger.Warn("empty stream from upstream, trying next target",
				"model", target.Model, "provider", target.Provider)
			return false
		}
		return true
	case actionResponse:
		result := rl.p.handleNonStreamingResponse(rl.gw, rl.w, rl.r, target, rl.opts.AgentName, action, rl.opts.CacheKey, rl.opts.Cooldown)
		if result.empty {
			rl.ctxLogger.Warn("empty non-streaming response from upstream, trying next target",
				"model", target.Model, "provider", target.Provider)
			return false
		}
		return true
	default:
		return false
	}
}

// newRetryLoop creates a retryLoop with the given parameters.
func newRetryLoop(p *Proxy, gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, opts forwardOptions) (*retryLoop, error) {
	ctxLogger := gw.Logger.With("operation", "forward", "agent", opts.AgentName, "api_key", opts.KeyRef)

	originalPayload, err := json.Marshal(opts.Payload)
	if err != nil {
		ctxLogger.Error("failed to marshal original payload for retry loop", "err", err)
		return nil, err
	}

	if (gw.Config.Compaction.Enabled != nil && *gw.Config.Compaction.Enabled) && gw.Config.Compaction.JSONMinify != nil && *gw.Config.Compaction.JSONMinify {
		minified := bytes.NewBuffer(make([]byte, 0, len(originalPayload)))
		err = json.Compact(minified, originalPayload)
		if err != nil {
			ctxLogger.Warn("failed to minify JSON payload, using original", "err", err)
		} else {
			originalPayload = minified.Bytes()
		}
	}

	return &retryLoop{
		p:               p,
		gw:              gw,
		w:               w,
		r:               r,
		opts:            opts,
		ctxLogger:       ctxLogger,
		originalPayload: originalPayload,
		stream:          opts.Stream,
	}, nil
}

// retrySignal controls the outer loop from action handlers.
type retrySignal int

const (
	retrySignalContinue retrySignal = iota
	retrySignalBreak
	retrySignalDone
)

// handleActionError processes an upstream error action, applies backoff, and returns a loop signal.
func (rl *retryLoop) handleActionError(i int, target routing.UpstreamTarget, action upstreamAction) retrySignal {
	rl.attempt++
	action.body, _ = io.ReadAll(io.LimitReader(action.resp.Body, pipeline.MaxErrorBodyBytes))
	_ = action.resp.Body.Close()
	shouldRetry, retryDelay := rl.handleUpstreamError(i, target, action)
	action.cancel()
	if !shouldRetry {
		http.Error(rl.w, "Upstream provider error", action.resp.StatusCode)
		return retrySignalDone
	}
	if rl.opts.MaxRetries > 0 && rl.attempt >= rl.opts.MaxRetries {
		return retrySignalBreak
	}
	if retryDelay > 0 {
		rl.ctxLogger.Info("retrying with parsed delay",
			"model", target.Model, "delay_ms", retryDelay.Milliseconds())
		waitWithCancel(rl.r.Context(), retryDelay)
	} else {
		backoff := calculateBackoff(rl.attempt - 1)
		rl.ctxLogger.Info("retrying with exponential backoff",
			"model", target.Model, "attempt", rl.attempt, "delay_ms", backoff.Milliseconds())
		waitWithCancel(rl.r.Context(), backoff)
	}
	return retrySignalContinue
}

// Run executes the retry loop. It returns true when the request has been fully
// handled (streaming or non-streaming success, or a terminal HTTP error response
// written to the client). It returns false when all targets are exhausted
// without sending a complete response, so the caller should respond with 503.
func (rl *retryLoop) Run() bool {
	defer rl.trackInFlight()
	workingPayload := make(map[string]interface{}, 16)
retryLoop:
	for i, target := range rl.opts.Targets {
		if rl.opts.MaxRetries > 0 && rl.attempt >= rl.opts.MaxRetries {
			rl.ctxLogger.Warn("max retries reached", "attempt", rl.attempt, "max", rl.opts.MaxRetries)
			break retryLoop
		}

		if !rl.copyPayload(workingPayload, i) {
			continue
		}

		action := rl.prepareAndSend(i, target, workingPayload)
		if !rl.handleActionResult(i, target, action) {
			continue
		}
		return true
	}
	return false
}

// prepareAndSend wraps the proxy's prepareAndSend method.
func (rl *retryLoop) prepareAndSend(idx int, target routing.UpstreamTarget, payload map[string]interface{}) upstreamAction {
	return rl.p.prepareAndSend(rl.gw, rl.r, idx, rl.opts.Targets, target, payload, rl.opts.Cooldown, rl.opts.TokenCount, rl.opts.AgentName, rl.stream)
}

// handleUpstreamError wraps the proxy's handleUpstreamError method.
func (rl *retryLoop) handleUpstreamError(idx int, target routing.UpstreamTarget, action upstreamAction) (bool, time.Duration) {
	return rl.p.handleUpstreamError(rl.gw, idx, rl.opts.Targets, target, rl.opts.Cooldown, rl.opts.AgentName, action)
}

// Exhausted signals that all targets have been exhausted.
func (rl *retryLoop) Exhausted() {
	rl.ctxLogger.Error("all upstream targets exhausted", "total", len(rl.opts.Targets), "attempts", rl.attempt)
	if rl.opts.AgentName != "" {
		rl.gw.Metrics.RecordExhausted(rl.opts.AgentName)
	}
	http.Error(rl.w, "All upstream targets exhausted", http.StatusServiceUnavailable)
}

// forwardToUpstream processes chat completion requests with retry logic.
func (p *Proxy) forwardToUpstream(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, opts forwardOptions) {
	rl, err := newRetryLoop(p, gw, w, r, opts)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if rl.Run() {
		return
	}

	rl.Exhausted()
}

func logRequestIfDebug(ctx context.Context, logger *slog.Logger, req *http.Request, targetURL string, body []byte) {
	if !logger.Enabled(ctx, slog.LevelDebug) {
		return
	}
	debugHeaders := make(http.Header)
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "key") || strings.Contains(lk, "auth") {
			debugHeaders[k] = []string{"[REDACTED]"}
		} else {
			debugHeaders[k] = v
		}
	}
	logger.Debug("forwarding to upstream", "url", targetURL)
	logger.Debug("request headers", "headers", debugHeaders)
	if len(body) > 0 && len(body) < 1000 {
		logger.Debug("request body", "body", string(body))
	}
}

func handleUpstreamResponse(ctxLogger *slog.Logger, resp *http.Response, cancel context.CancelFunc, stream bool) upstreamAction {
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(strings.ToLower(ct), "text/html") {
		ctxLogger.Warn("upstream returned HTML instead of API response, skipping target",
			"content_type", ct, "status", resp.StatusCode)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		cancel()
		return upstreamAction{kind: actionContinue}
	}
	if stream {
		return upstreamAction{kind: actionStream, resp: resp, cancel: cancel}
	}
	return upstreamAction{kind: actionResponse, resp: resp, cancel: cancel}
}

func (p *Proxy) prepareAndSend(gw *gateway.NenyaGateway,
	r *http.Request,
	idx int,
	targets []routing.UpstreamTarget,
	target routing.UpstreamTarget,
	payload map[string]interface{},
	cooldownDuration time.Duration,
	tokenCount int,
	agentName string,
	stream bool,
) upstreamAction {
	ctxLogger := gw.Logger.With(
		"operation", "upstream",
		"agent", agentName,
		"provider", target.Provider,
		"model", target.Model,
		"target_idx", fmt.Sprintf("%d/%d", idx+1, len(targets)),
	)

	gw.Stats.RecordRequest(target.Model, tokenCount)
	gw.Metrics.RecordTokens("input", target.Model, agentName, target.Provider, tokenCount)
	gw.Metrics.RecordUpstreamRequest(target.Model, agentName, target.Provider)

	if !gw.RateLimiter.Check(target.URL, tokenCount) {
		gw.Metrics.RecordRateLimitRejected(infra.ExtractHost(target.URL))
		ctxLogger.Warn("target skipped: rate limit exceeded")
		return upstreamAction{kind: actionContinue}
	}

	if target.CoolKey != "" && !gw.AgentState.CB.Allow(target.CoolKey) {
		ctxLogger.Warn("target skipped: circuit breaker open")
		return upstreamAction{kind: actionContinue}
	}

	transformDeps := routing.TransformDeps{
		Logger:             gw.Logger,
		Providers:          gw.Providers,
		Config:             &gw.Config,
		ThoughtSigCache:    gw.ThoughtSigCache,
		ExtractContentText: gateway.ExtractContentText,
		Catalog:            gw.ModelCatalog,
	}
	transformedBody, _, err := routing.TransformRequestForUpstream(transformDeps, target.Provider, target.URL, payload, target.Model, target.MaxOutput, target.Format)
	if err != nil {
		ctxLogger.Warn("failed to transform request, using original payload", "err", err)
		transformedBody, _ = json.Marshal(payload)
	}

	req, err := p.buildUpstreamRequest(gw, r.Context(), r.Method, target.URL, transformedBody, target.Provider, r.Header)
	if err != nil {
		ctxLogger.Error("failed to create upstream request", "err", err)
		return upstreamAction{kind: actionContinue}
	}

	logRequestIfDebug(r.Context(), ctxLogger, req, target.URL, transformedBody)

	var upstreamCtx context.Context
	var upstreamCancel context.CancelFunc
	if pr, ok := gw.Providers[target.Provider]; ok && pr.TimeoutSeconds > 0 {
		upstreamCtx, upstreamCancel = context.WithTimeout(r.Context(), time.Duration(pr.TimeoutSeconds)*time.Second)
	} else {
		upstreamCtx, upstreamCancel = context.WithCancel(r.Context())
	}
	req = req.WithContext(upstreamCtx)

	startTime := time.Now()
	resp, err := gw.Client.Do(req)
	if err != nil {
		upstreamCancel()
		ctxLogger.Warn("target network error", "err", err)
		gw.AgentState.RecordFailure(target, cooldownDuration)
		return upstreamAction{kind: actionContinue}
	}

	duration := time.Since(startTime)
	gw.Metrics.RecordUpstreamLatency(target.Model, agentName, target.Provider, duration)
	if gw.LatencyTracker != nil {
		gw.LatencyTracker.Record(target.Model, target.Provider, duration)
	}

	ctxLogger.Info("upstream response", "status", resp.StatusCode)

	if resp.StatusCode >= 400 {
		gw.Stats.RecordError(target.Model)
		if gw.CostTracker != nil {
			gw.CostTracker.RecordError(target.Model)
		}
		gw.Metrics.RecordUpstreamError(target.Model, agentName, target.Provider, resp.StatusCode)
		return upstreamAction{kind: actionError, resp: resp, cancel: upstreamCancel}
	}

	return handleUpstreamResponse(ctxLogger, resp, upstreamCancel, stream)
}

func logRetryableError(ctxLogger *slog.Logger, errorBody []byte, gw *gateway.NenyaGateway) {
	if len(errorBody) > 0 {
		logBody := pipeline.RedactSecrets(string(errorBody), (gw.Config.Bouncer.Enabled != nil && *gw.Config.Bouncer.Enabled), gw.SecretPatterns, gw.Config.Bouncer.RedactionLabel)
		if len(logBody) > 512 {
			logBody = logBody[:512] + "...[truncated]"
		}
		ctxLogger.Warn("upstream retryable error", "body", logBody)
	} else {
		ctxLogger.Warn("retryable error, trying next target")
	}
}

func handleRetryableError429(logger *slog.Logger, errorBody []byte, action upstreamAction, cooldownDuration time.Duration, target routing.UpstreamTarget, agentName string, gw *gateway.NenyaGateway) time.Duration {
	effectiveCooldown := cooldownDuration
	if action.resp.StatusCode == http.StatusTooManyRequests && len(errorBody) > 0 {
		if quotaCD := parseQuotaExhaustion(errorBody); quotaCD > 0 {
			if quotaCD > cooldownDuration {
				effectiveCooldown = quotaCD
				logger.Info("quota exhaustion detected, extending cooldown", "cooldown_s", effectiveCooldown.Seconds())
			}
		}
	}

	if action.resp.StatusCode == http.StatusTooManyRequests {
		gw.AgentState.ActivateCooldown(target, effectiveCooldown)
		gw.Metrics.RecordCooldown(agentName, target.Provider, target.Model)
		return parseRetryDelay(action.resp.Header, errorBody)
	}

	gw.AgentState.RecordFailure(target, effectiveCooldown)
	return 0
}

func handleAdapterRetryableError(ctxLogger *slog.Logger, target routing.UpstreamTarget, action upstreamAction, cooldownDuration time.Duration, gw *gateway.NenyaGateway) bool {
	a := adapter.ForProvider(target.Provider)
	errClass := a.NormalizeError(action.resp.StatusCode, action.body)
	if (errClass == adapter.ErrorRetryable || errClass == adapter.ErrorQuotaExhausted) && action.resp.StatusCode >= 400 && action.resp.StatusCode < 500 {
		ctxLogger.Warn("adapter classified client error as retryable, trying next target", "error_class", errClass)
		gw.AgentState.RecordFailure(target, cooldownDuration)
		return true
	}
	return false
}

func (p *Proxy) handleUpstreamError(gw *gateway.NenyaGateway,
	idx int,
	targets []routing.UpstreamTarget,
	target routing.UpstreamTarget,
	cooldownDuration time.Duration,
	agentName string,
	action upstreamAction,
) (bool, time.Duration) {
	errorBody := action.body

	ctxLogger := gw.Logger.With(
		"operation", "upstream_error",
		"agent", agentName,
		"provider", target.Provider,
		"model", target.Model,
		"target_idx", fmt.Sprintf("%d/%d", idx+1, len(targets)),
		"status", action.resp.StatusCode,
	)

	if p.isRetryableStatus(gw, target.Provider, action.resp.StatusCode) {
		logRetryableError(ctxLogger, errorBody, gw)
		delay := handleRetryableError429(ctxLogger, errorBody, action, cooldownDuration, target, agentName, gw)
		return true, delay
	}

	if isRetryableClientErrorForProvider(action.resp.StatusCode, errorBody, target.Provider) && len(targets) > 1 {
		logBody := redactForLog(string(errorBody), gw)
		ctxLogger.Warn("retryable client error from upstream, trying next target", "body", logBody)
		gw.AgentState.RecordFailure(target, cooldownDuration)
		return true, 0
	}

	if handleAdapterRetryableError(ctxLogger, target, action, cooldownDuration, gw) {
		return true, 0
	}

	defer action.cancel()
	if len(targets) > 1 {
		logWarnRetryable(ctxLogger, errorBody, gw, "non-retryable upstream error, trying next target")
		return true, 0
	}

	logErrorRetryable(ctxLogger, errorBody, gw, "non-retryable upstream error, no more targets")
	return false, 0
}

func logWarnRetryable(ctxLogger *slog.Logger, errorBody []byte, gw *gateway.NenyaGateway, msg string) {
	if len(errorBody) == 0 {
		ctxLogger.Warn(msg + " (empty body)")
		return
	}
	ctxLogger.Warn(msg, "body", redactForLog(string(errorBody), gw))
}

func logErrorRetryable(ctxLogger *slog.Logger, errorBody []byte, gw *gateway.NenyaGateway, msg string) {
	if len(errorBody) == 0 {
		ctxLogger.Error(msg + " (empty body)")
		return
	}
	ctxLogger.Error(msg, "body", redactForLog(string(errorBody), gw))
}

// redactForLog applies secret redaction and truncation to error body text before
// writing to logs, preventing upstream error responses from leaking secrets.
func redactForLog(body string, gw *gateway.NenyaGateway) string {
	s := pipeline.RedactSecrets(body, (gw.Config.Bouncer.Enabled != nil && *gw.Config.Bouncer.Enabled), gw.SecretPatterns, gw.Config.Bouncer.RedactionLabel)
	if len(s) > 512 {
		s = s[:512] + "...[truncated]"
	}
	return s
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

func (p *Proxy) isRetryableStatus(gw *gateway.NenyaGateway, providerName string, statusCode int) bool {
	if pr, ok := gw.Providers[providerName]; ok && len(pr.RetryableStatusCodes) > 0 {
		return routing.SliceContains(pr.RetryableStatusCodes, statusCode)
	}
	if len(gw.Config.Governance.RetryableStatusCodes) > 0 {
		return routing.SliceContains(gw.Config.Governance.RetryableStatusCodes, statusCode)
	}
	return routing.SliceContains(defaultRetryableStatusCodes, statusCode)
}

var defaultRetryableStatusCodes = []int{
	http.StatusTooManyRequests,
	http.StatusInternalServerError,
	http.StatusBadGateway,
	http.StatusServiceUnavailable,
	http.StatusGatewayTimeout,
}

var commonRetryablePatterns = []string{
	"unavailable_model",
	"tokens_limit_reached",
	"context_length_exceeded",
	"context length",
	"model_overloaded",
	"overloaded",
	"thought_signature",
	"name cannot be empty",
	"messages parameter is illegal",
	"unknown_model",
	"max_tokens",
	"rate_limit_exceeded",
	"extra_forbidden",
	"enable-auto-tool-choice",
	"tool_call_parser",
	"valid string",
	"resource_exhausted",
	"quota exceeded",
	"quota_exceeded",
}

var deepseekRetryablePatterns = []string{
	"reasoning_content",
	"thinking mode",
}

var anthropicRetryablePatterns = []string{
	"overloaded_error",
	"prompt is too long",
	"prompt: length",
}

var geminiRetryablePatterns = []string{
	"the response was blocked",
	"content has no parts",
}

type providerMatcher struct {
	name     string
	patterns []string
}

var providerMatchers = []providerMatcher{
	{name: "anthropic", patterns: anthropicRetryablePatterns},
	{name: "gemini", patterns: geminiRetryablePatterns},
	{name: "vertex", patterns: geminiRetryablePatterns},
	{name: "deepseek", patterns: deepseekRetryablePatterns},
}

func matchProviderSpecificPatterns(lowerBody string, provider string) bool {
	lp := strings.ToLower(provider)
	for _, m := range providerMatchers {
		if strings.Contains(lp, m.name) {
			for _, pat := range m.patterns {
				if strings.Contains(lowerBody, pat) {
					return true
				}
			}
		}
	}
	return false
}

func isRetryableClientErrorForProvider(statusCode int, body []byte, provider string) bool {
	if statusCode != http.StatusBadRequest && statusCode != http.StatusRequestEntityTooLarge && statusCode != http.StatusUnprocessableEntity {
		return false
	}
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))

	for _, pat := range commonRetryablePatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}

	return matchProviderSpecificPatterns(lower, provider)
}

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

func parseRetryDelayFromErrorObject(body []byte) time.Duration {
	var envelope struct {
		Error struct {
			Details []rpcDetail `json:"details"`
			Message string      `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0
	}
	if d := parseRetryDelayFromRPCDetails(envelope.Error.Details); d > 0 {
		return d
	}
	if envelope.Error.Message == "" {
		return 0
	}
	return parseRetryDelayFromMessage(envelope.Error.Message)
}

func parseRetryDelayFromErrorArray(body []byte) time.Duration {
	var arr []struct {
		Error struct {
			Details []rpcDetail `json:"details"`
			Message string      `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &arr); err != nil || len(arr) == 0 {
		return 0
	}
	if d := parseRetryDelayFromRPCDetails(arr[0].Error.Details); d > 0 {
		return d
	}
	if arr[0].Error.Message == "" {
		return 0
	}
	return parseRetryDelayFromMessage(arr[0].Error.Message)
}

func parseRetryAfterHeader(header http.Header) time.Duration {
	v := header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := time.ParseDuration(v + "s"); err == nil && secs > 0 {
		return capRetryDelay(secs)
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return capRetryDelay(d)
		}
	}
	return 0
}

func parseRetryDelay(header http.Header, body []byte) time.Duration {
	if d := parseRetryAfterHeader(header); d > 0 {
		return d
	}
	if len(body) == 0 {
		return 0
	}
	if d := parseRetryDelayFromErrorObject(body); d > 0 {
		return d
	}
	return parseRetryDelayFromErrorArray(body)
}
