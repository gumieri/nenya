package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nenya/internal/gateway"
	"nenya/internal/infra"
	"nenya/internal/pipeline"
	"nenya/internal/routing"
)

const maxRetryBackoff = 5 * time.Second
const maxQuotaCooldown = 30 * time.Minute

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

func (p *Proxy) forwardToUpstream(
	w http.ResponseWriter,
	r *http.Request,
	targets []routing.UpstreamTarget,
	payload map[string]interface{},
	cooldownDuration time.Duration,
	tokenCount int,
	agentName string,
) {
	originalPayload, err := json.Marshal(payload)
	if err != nil {
		p.GW.Logger.Error("failed to marshal original payload for retry loop", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if p.GW.Config.Compaction.Enabled && p.GW.Config.Compaction.JSONMinify {
		minified := bytes.NewBuffer(make([]byte, 0, len(originalPayload)))
		err = json.Compact(minified, originalPayload)
		if err != nil {
			p.GW.Logger.Warn("failed to minify JSON payload, using original", "err", err)
		} else {
			originalPayload = minified.Bytes()
		}
	}

	for i, target := range targets {
		workingPayload := make(map[string]interface{})
		if err := json.Unmarshal(originalPayload, &workingPayload); err != nil {
			p.GW.Logger.Error("failed to unmarshal payload for target",
				"target", i+1, "total", len(targets), "err", err)
			continue
		}

		action := p.prepareAndSend(r, i, targets, target, workingPayload, cooldownDuration, tokenCount, agentName)
		switch action.kind {
		case actionContinue:
			continue
		case actionError:
			action.body, _ = io.ReadAll(io.LimitReader(action.resp.Body, pipeline.MaxErrorBodyBytes))
			action.resp.Body.Close()
			shouldContinue := p.handleUpstreamError(r, i, targets, target, cooldownDuration, agentName, action)
			action.cancel()
			if shouldContinue {
				continue
			}
			http.Error(w, "Upstream provider error", action.resp.StatusCode)
			return
		case actionStream:
			p.streamResponse(w, r, target, agentName, action)
			return
		}
	}

	p.GW.Logger.Error("all upstream targets exhausted", "total", len(targets))
	if p.GW.Metrics != nil && agentName != "" {
		p.GW.Metrics.RecordExhausted(agentName)
	}
	http.Error(w, "All upstream targets exhausted", http.StatusServiceUnavailable)
}

func (p *Proxy) prepareAndSend(
	r *http.Request,
	idx int,
	targets []routing.UpstreamTarget,
	target routing.UpstreamTarget,
	payload map[string]interface{},
	cooldownDuration time.Duration,
	tokenCount int,
	agentName string,
) upstreamAction {
	p.GW.Stats.RecordRequest(target.Model, tokenCount)
	if p.GW.Metrics != nil {
		p.GW.Metrics.RecordTokens("input", target.Model, agentName, target.Provider, tokenCount)
		p.GW.Metrics.RecordUpstreamRequest(target.Model, agentName, target.Provider)
	}

	if !p.GW.RateLimiter.Check(target.URL, tokenCount) {
		if p.GW.Metrics != nil {
			p.GW.Metrics.RecordRateLimitRejected(infra.ExtractHost(target.URL))
		}
		p.GW.Logger.Warn("target skipped: rate limit exceeded",
			"target", idx+1, "total", len(targets), "model", target.Model)
		return upstreamAction{kind: actionContinue}
	}

	transformDeps := routing.TransformDeps{
		Logger:             p.GW.Logger,
		Providers:          p.GW.Providers,
		Config:             &p.GW.Config,
		ThoughtSigCache:    p.GW.ThoughtSigCache,
		ExtractContentText: gateway.ExtractContentText,
	}
	transformedBody, _, err := routing.TransformRequestForUpstream(transformDeps, target.Provider, target.URL, payload, target.Model, target.MaxOutput)
	if err != nil {
		p.GW.Logger.Warn("failed to transform request, using original payload",
			"target", idx+1, "total", len(targets), "model", target.Model, "err", err)
		transformedBody, _ = json.Marshal(payload)
	}

	req, err := p.buildUpstreamRequest(r.Context(), r.Method, target.URL, transformedBody, target.Provider, r.Header)
	if err != nil {
		p.GW.Logger.Error("failed to create upstream request",
			"target", idx+1, "total", len(targets), "err", err)
		return upstreamAction{kind: actionContinue}
	}

	if p.GW.Logger.Enabled(r.Context(), slog.LevelDebug) {
		debugHeaders := make(http.Header)
		for k, v := range req.Header {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "key") || strings.Contains(lk, "auth") {
				debugHeaders[k] = []string{"[REDACTED]"}
			} else {
				debugHeaders[k] = v
			}
		}
		p.GW.Logger.Debug("forwarding to upstream",
			"url", target.URL, "target", idx+1, "total", len(targets))
		p.GW.Logger.Debug("request headers", "headers", debugHeaders)
		if len(transformedBody) > 0 && len(transformedBody) < 1000 {
			p.GW.Logger.Debug("request body", "body", string(transformedBody))
		}
	}

	upstreamCtx, upstreamCancel := context.WithCancel(r.Context())
	req = req.WithContext(upstreamCtx)

	resp, err := p.GW.Client.Do(req)
	if err != nil {
		upstreamCancel()
		p.GW.Logger.Warn("target network error",
			"target", idx+1, "total", len(targets), "model", target.Model, "err", err)
		return upstreamAction{kind: actionContinue}
	}

	p.GW.Logger.Info("upstream response",
		"target", idx+1, "total", len(targets), "model", target.Model, "status", resp.StatusCode)

	if resp.StatusCode >= 400 {
		p.GW.Stats.RecordError(target.Model)
		if p.GW.Metrics != nil {
			p.GW.Metrics.RecordUpstreamError(target.Model, agentName, target.Provider, resp.StatusCode)
		}
		return upstreamAction{kind: actionError, resp: resp, cancel: upstreamCancel}
	}

	return upstreamAction{kind: actionStream, resp: resp, cancel: upstreamCancel}
}

func (p *Proxy) handleUpstreamError(
	r *http.Request,
	idx int,
	targets []routing.UpstreamTarget,
	target routing.UpstreamTarget,
	cooldownDuration time.Duration,
	agentName string,
	action upstreamAction,
) bool {
	errorBody := action.body

	if p.isRetryableStatus(target.Provider, action.resp.StatusCode) {
		if len(errorBody) > 0 {
			p.GW.Logger.Warn("upstream retryable error",
				"target", idx+1, "total", len(targets), "model", target.Model,
				"status", action.resp.StatusCode, "body", string(errorBody))
		} else {
			p.GW.Logger.Warn("activating cooldown, trying next target",
				"target", idx+1, "total", len(targets), "model", target.Model, "status", action.resp.StatusCode)
		}
		effectiveCooldown := cooldownDuration
		if action.resp.StatusCode == http.StatusTooManyRequests && len(errorBody) > 0 {
			if quotaCD := parseQuotaExhaustion(errorBody); quotaCD > 0 {
				if quotaCD > cooldownDuration {
					effectiveCooldown = quotaCD
					p.GW.Logger.Info("quota exhaustion detected, extending cooldown",
						"model", target.Model, "cooldown_s", effectiveCooldown.Seconds())
				}
			}
		}
		p.GW.AgentState.ActivateCooldown(target, effectiveCooldown)
		if p.GW.Metrics != nil {
			p.GW.Metrics.RecordCooldown(agentName, target.Provider, target.Model)
		}
		if action.resp.StatusCode == http.StatusTooManyRequests {
			delay := parseRetryDelay(action.resp.Header, errorBody)
			if delay > 0 {
				p.GW.Logger.Info("rate limited, backing off before retry",
					"model", target.Model, "delay_ms", delay.Milliseconds())
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
		p.GW.Logger.Warn("retryable client error from upstream, trying next target",
			"target", idx+1, "total", len(targets), "model", target.Model,
			"status", action.resp.StatusCode, "body", string(errorBody))
		p.GW.AgentState.ActivateCooldown(target, cooldownDuration)
		return true
	}

	defer action.cancel()
	if len(errorBody) > 0 {
		p.GW.Logger.Error("non-retryable upstream error",
			"target", idx+1, "total", len(targets), "model", target.Model,
			"status", action.resp.StatusCode, "body", string(errorBody))
	} else {
		p.GW.Logger.Error("non-retryable upstream error, empty body",
			"target", idx+1, "total", len(targets), "model", target.Model, "status", action.resp.StatusCode)
	}
	return false
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

func (p *Proxy) isRetryableStatus(providerName string, statusCode int) bool {
	if pr, ok := p.GW.Providers[providerName]; ok && len(pr.RetryableStatusCodes) > 0 {
		return routing.SliceContains(pr.RetryableStatusCodes, statusCode)
	}
	if len(p.GW.Config.Governance.RetryableStatusCodes) > 0 {
		return routing.SliceContains(p.GW.Config.Governance.RetryableStatusCodes, statusCode)
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

func isRetryableClientError(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest && statusCode != http.StatusRequestEntityTooLarge && statusCode != http.StatusUnprocessableEntity {
		return false
	}
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "unavailable_model") ||
		strings.Contains(lower, "tokens_limit_reached") ||
		strings.Contains(lower, "context_length_exceeded") ||
		strings.Contains(lower, "context length") ||
		strings.Contains(lower, "model_overloaded") ||
		strings.Contains(lower, "overloaded") ||
		strings.Contains(lower, "thought_signature") ||
		strings.Contains(lower, "name cannot be empty") ||
		strings.Contains(lower, "messages parameter is illegal") ||
		strings.Contains(lower, "unknown_model") ||
		strings.Contains(lower, "max_tokens") ||
		strings.Contains(lower, "extra_forbidden") ||
		strings.Contains(lower, "enable-auto-tool-choice") ||
		strings.Contains(lower, "tool_call_parser") ||
		strings.Contains(lower, "valid string")
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
