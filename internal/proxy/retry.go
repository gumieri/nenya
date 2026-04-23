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

func (p *Proxy) forwardToUpstream(gw *gateway.NenyaGateway,
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
	ctxLogger := gw.Logger.With("operation", "forward", "agent", agentName)

	originalPayload, err := json.Marshal(payload)
	if err != nil {
		ctxLogger.Error("failed to marshal original payload for retry loop", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if gw.Config.Compaction.Enabled && gw.Config.Compaction.JSONMinify {
		minified := bytes.NewBuffer(make([]byte, 0, len(originalPayload)))
		err = json.Compact(minified, originalPayload)
		if err != nil {
			ctxLogger.Warn("failed to minify JSON payload, using original", "err", err)
		} else {
			originalPayload = minified.Bytes()
		}
	}

	attempt := 0
retryLoop:
	for i, target := range targets {
		if maxRetries > 0 && attempt >= maxRetries {
			ctxLogger.Warn("max retries reached", "attempt", attempt, "max", maxRetries)
			break retryLoop
		}

		workingPayload := make(map[string]interface{})
		if err := json.Unmarshal(originalPayload, &workingPayload); err != nil {
			ctxLogger.Error("failed to unmarshal payload for target",
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
			shouldRetry, retryDelay := p.handleUpstreamError(gw, i, targets, target, cooldownDuration, agentName, action)
			action.cancel()
			if shouldRetry {
				if maxRetries > 0 && attempt >= maxRetries {
					break retryLoop
				}
				if retryDelay > 0 {
					ctxLogger.Info("retrying with parsed delay",
						"model", target.Model, "delay_ms", retryDelay.Milliseconds())
					waitWithCancel(r.Context(), retryDelay)
				} else {
					backoff := calculateBackoff(attempt - 1)
					ctxLogger.Info("retrying with exponential backoff",
						"model", target.Model, "attempt", attempt, "delay_ms", backoff.Milliseconds())
					waitWithCancel(r.Context(), backoff)
				}
				continue
			}
			http.Error(w, "Upstream provider error", action.resp.StatusCode)
			return
		case actionStream:
			p.streamResponse(gw, w, r, target, agentName, action, cacheKey, cooldownDuration)
			return
		}
	}

	ctxLogger.Error("all upstream targets exhausted", "total", len(targets), "attempts", attempt)
	if gw.Metrics != nil && agentName != "" {
		gw.Metrics.RecordExhausted(agentName)
	}
	http.Error(w, "All upstream targets exhausted", http.StatusServiceUnavailable)
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
) upstreamAction {
	ctxLogger := gw.Logger.With(
		"operation", "upstream",
		"agent", agentName,
		"provider", target.Provider,
		"model", target.Model,
		"target_idx", fmt.Sprintf("%d/%d", idx+1, len(targets)),
	)

	gw.Stats.RecordRequest(target.Model, tokenCount)
	if gw.Metrics != nil {
		gw.Metrics.RecordTokens("input", target.Model, agentName, target.Provider, tokenCount)
		gw.Metrics.RecordUpstreamRequest(target.Model, agentName, target.Provider)
	}

	if !gw.RateLimiter.Check(target.URL, tokenCount) {
		if gw.Metrics != nil {
			gw.Metrics.RecordRateLimitRejected(infra.ExtractHost(target.URL))
		}
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
	}
	transformedBody, _, err := routing.TransformRequestForUpstream(transformDeps, target.Provider, target.URL, payload, target.Model, target.MaxOutput)
	if err != nil {
		ctxLogger.Warn("failed to transform request, using original payload", "err", err)
		transformedBody, _ = json.Marshal(payload)
	}

	req, err := p.buildUpstreamRequest(gw, r.Context(), r.Method, target.URL, transformedBody, target.Provider, r.Header)
	if err != nil {
		ctxLogger.Error("failed to create upstream request", "err", err)
		return upstreamAction{kind: actionContinue}
	}

	if gw.Logger.Enabled(r.Context(), slog.LevelDebug) {
		debugHeaders := make(http.Header)
		for k, v := range req.Header {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "key") || strings.Contains(lk, "auth") {
				debugHeaders[k] = []string{"[REDACTED]"}
			} else {
				debugHeaders[k] = v
			}
		}
		ctxLogger.Debug("forwarding to upstream", "url", target.URL)
		ctxLogger.Debug("request headers", "headers", debugHeaders)
		if len(transformedBody) > 0 && len(transformedBody) < 1000 {
			ctxLogger.Debug("request body", "body", string(transformedBody))
		}
	}

	var upstreamCtx context.Context
	var upstreamCancel context.CancelFunc
	if pr, ok := gw.Providers[target.Provider]; ok && pr.TimeoutSeconds > 0 {
		upstreamCtx, upstreamCancel = context.WithTimeout(r.Context(), time.Duration(pr.TimeoutSeconds)*time.Second)
	} else {
		upstreamCtx, upstreamCancel = context.WithCancel(r.Context())
	}
	req = req.WithContext(upstreamCtx)

	resp, err := gw.Client.Do(req)
	if err != nil {
		upstreamCancel()
		ctxLogger.Warn("target network error", "err", err)
		return upstreamAction{kind: actionContinue}
	}

	ctxLogger.Info("upstream response", "status", resp.StatusCode)

	if resp.StatusCode >= 400 {
		gw.Stats.RecordError(target.Model)
		if gw.Metrics != nil {
			gw.Metrics.RecordUpstreamError(target.Model, agentName, target.Provider, resp.StatusCode)
		}
		return upstreamAction{kind: actionError, resp: resp, cancel: upstreamCancel}
	}

	return upstreamAction{kind: actionStream, resp: resp, cancel: upstreamCancel}
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
		if len(errorBody) > 0 {
			logBody := pipeline.RedactSecrets(string(errorBody), gw.Config.SecurityFilter.Enabled, gw.SecretPatterns, gw.Config.SecurityFilter.RedactionLabel)
			if len(logBody) > 512 {
				logBody = logBody[:512] + "...[truncated]"
			}
			ctxLogger.Warn("upstream retryable error", "body", logBody)
		} else {
			ctxLogger.Warn("retryable error, trying next target")
		}
		effectiveCooldown := cooldownDuration
		if action.resp.StatusCode == http.StatusTooManyRequests && len(errorBody) > 0 {
			if quotaCD := parseQuotaExhaustion(errorBody); quotaCD > 0 {
				if quotaCD > cooldownDuration {
					effectiveCooldown = quotaCD
					ctxLogger.Info("quota exhaustion detected, extending cooldown", "cooldown_s", effectiveCooldown.Seconds())
				}
			}
		}

		if action.resp.StatusCode == http.StatusTooManyRequests {
			gw.AgentState.ActivateCooldown(target, effectiveCooldown)
			if gw.Metrics != nil {
				gw.Metrics.RecordCooldown(agentName, target.Provider, target.Model)
			}
			delay := parseRetryDelay(action.resp.Header, errorBody)
			return true, delay
		}

		gw.AgentState.RecordFailure(target, effectiveCooldown)
		return true, 0
	}

	if isRetryableClientErrorForProvider(action.resp.StatusCode, errorBody, target.Provider) && len(targets) > 1 {
		logBody := redactForLog(string(errorBody), gw)
		ctxLogger.Warn("retryable client error from upstream, trying next target", "body", logBody)
		gw.AgentState.RecordFailure(target, cooldownDuration)
		return true, 0
	}

	a := adapter.ForProvider(target.Provider)
	errClass := a.NormalizeError(action.resp.StatusCode, errorBody)
	if (errClass == adapter.ErrorRetryable || errClass == adapter.ErrorQuotaExhausted) && action.resp.StatusCode >= 400 && action.resp.StatusCode < 500 {
		ctxLogger.Warn("adapter classified client error as retryable, trying next target", "error_class", errClass)
		gw.AgentState.RecordFailure(target, cooldownDuration)
		return true, 0
	}

	defer action.cancel()
	if len(targets) > 1 {
		if len(errorBody) > 0 {
			logBody := redactForLog(string(errorBody), gw)
			ctxLogger.Warn("non-retryable upstream error, trying next target", "body", logBody)
		} else {
			ctxLogger.Warn("non-retryable upstream error (empty body), trying next target")
		}
		return true, 0
	}

	if len(errorBody) > 0 {
		logBody := redactForLog(string(errorBody), gw)
		ctxLogger.Error("non-retryable upstream error, no more targets", "body", logBody)
	} else {
		ctxLogger.Error("non-retryable upstream error (empty body), no more targets")
	}
	return false, 0
}

// redactForLog applies secret redaction and truncation to error body text before
// writing to logs, preventing upstream error responses from leaking secrets.
func redactForLog(body string, gw *gateway.NenyaGateway) string {
	s := pipeline.RedactSecrets(body, gw.Config.SecurityFilter.Enabled, gw.SecretPatterns, gw.Config.SecurityFilter.RedactionLabel)
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

var anthropicRetryablePatterns = []string{
	"overloaded_error",
	"prompt is too long",
	"prompt: length",
}

var geminiRetryablePatterns = []string{
	"the response was blocked",
	"content has no parts",
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

	lp := strings.ToLower(provider)
	if strings.Contains(lp, "anthropic") {
		for _, pat := range anthropicRetryablePatterns {
			if strings.Contains(lower, pat) {
				return true
			}
		}
	}
	if strings.Contains(lp, "gemini") || strings.Contains(lp, "vertex") {
		for _, pat := range geminiRetryablePatterns {
			if strings.Contains(lower, pat) {
				return true
			}
		}
	}

	return false
}

func isRetryableClientError(statusCode int, body []byte) bool {
	return isRetryableClientErrorForProvider(statusCode, body, "")
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
