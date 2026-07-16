package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nenya/internal/adapter"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/util"
)

const maxRetryBackoff = 5 * time.Second
const maxQuotaCooldown = 30 * time.Minute
const retrySystemPrompt = "Summarize the following conversation messages into a single coherent response. Preserve key context and important details. Remove redundant information."

const (
	// ZAI code 1308: "已达到5小时使用上限" (5-hour usage limit reached)
	zaiCode1308FallbackCooldown = 5 * time.Hour
	// ZAI code 1310: "已达到每周使用上限" (weekly usage limit reached)
	// Minimum 1-hour cooldown prevents retry storms on short windows
	zaiCode1310FallbackCooldown = 1 * time.Hour
	zaiCode1310MinCooldown      = 1 * time.Hour
)

const (
	exponentialBackoffBase   = 500 * time.Millisecond
	exponentialBackoffMax    = 8 * time.Second
	exponentialBackoffJitter = 750 * time.Millisecond
)

// upstreamAction holds the result of a single upstream HTTP request attempt.
// The kind field distinguishes between streaming, response, and error outcomes.
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
	for range attempt {
		delay *= 2
		if delay >= exponentialBackoffMax {
			delay = exponentialBackoffMax
			break
		}
	}
	jitter := time.Duration(rand.Int63n(int64(exponentialBackoffJitter)))
	return delay + jitter
}

// summarizeMessages compresses the messages array using the configured engine chain.
// It serializes all messages, sends them to the summarization engine, and returns
// a replacement message array with a single assistant message containing the summary.
// Only one summarization attempt is allowed per request to avoid loops.
func (p *Proxy) summarizeMessages(ctx context.Context, gw *gateway.NenyaGateway, messages []interface{}, agentName, providerName, modelName string) ([]interface{}, error) {
	if len(gw.Config.Bouncer.Engine.ResolvedTargets) == 0 {
		return nil, fmt.Errorf("engine chain not configured")
	}

	var textForSummary strings.Builder
	for _, msgRaw := range messages {
		if msgMap, ok := msgRaw.(map[string]interface{}); ok {
			fmt.Fprintf(&textForSummary, "%s: %s\n", msgMap["role"], gateway.ExtractContentText(msgMap))
		}
	}

	if textForSummary.Len() == 0 {
		return nil, fmt.Errorf("no text content to summarize")
	}

	start := time.Now()
	summary, err := pipeline.CallEngineChain(
		ctx, gw.Client, gw.OllamaClient,
		gw.Config.Bouncer.Engine.ResolvedTargets, gw.Logger,
		func(providerName string, headers http.Header) error {
			return routing.InjectAPIKeyWithGateway(providerName, gw, headers)
		},
		"context_limit_retry", gw.Config.Bouncer.Engine.AgentName, retrySystemPrompt, textForSummary.String())

	if gw.Metrics != nil {
		gw.Metrics.RecordSummarizationDuration(agentName, providerName, modelName, time.Since(start))
	}

	if err != nil {
		return nil, fmt.Errorf("summarization failed: %w", err)
	}

	return []interface{}{
		map[string]interface{}{
			"role":    "assistant",
			"content": summary,
		},
	}, nil
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

// doUpstreamRoundTrip executes a retried upstream HTTP round-trip.
// It builds the request, sets Content-Type (if non-empty), executes with retry,
// and returns the response. 5xx responses are retried.
func (p *Proxy) doUpstreamRoundTrip(ctx context.Context, gw *gateway.NenyaGateway, method, targetURL string, bodyBytes []byte, providerName, modelName string, srcHeaders http.Header, contentType string, maxAttempts int) (*http.Response, error) {
	return util.DoWithRetryResp(ctx, maxAttempts, func() (*http.Response, error) {
		upstreamReq, reqErr := p.buildUpstreamRequest(gw, ctx, method, targetURL, bodyBytes, providerName, modelName, "", srcHeaders)
		if reqErr != nil {
			return nil, reqErr
		}
		if contentType != "" {
			upstreamReq.Header.Set("Content-Type", contentType)
		}
		resp, fetchErr := gw.Client.Do(upstreamReq)
		if fetchErr != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}
			return nil, fetchErr
		}
		if resp.StatusCode >= 500 {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("upstream error: %d", resp.StatusCode)
		}
		return resp, nil
	})
}

// forwardOptions holds the parameters for forwarding a request upstream.
type forwardOptions struct {
	Targets      []routing.UpstreamTarget
	Payload      map[string]any
	Stream       bool
	Cooldown     time.Duration
	TokenCount   int
	AgentName    string
	MaxRetries   int
	CacheKey     string
	KeyRef       string
	SourceFormat string
}

// retryLoop encapsulates the state and logic for retrying upstream requests.
// Maintains attempt counter, quota exhaustion flag, and request context across retries.
type retryLoop struct {
	p                 *Proxy
	gw                *gateway.NenyaGateway
	w                 http.ResponseWriter
	r                 *http.Request
	opts              forwardOptions
	ctxLogger         *slog.Logger
	originalPayload   []byte
	stream            bool
	summarized        bool
	summarizedPayload map[string]interface{}
	attempt           int
	quotaExhausted    bool
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
		result := rl.p.streamResponse(rl.gw, rl.w, rl.r, target, rl.opts.AgentName, rl.opts.SourceFormat, action, rl.opts.CacheKey, rl.opts.Cooldown, rl.opts.Payload)
		if result.empty {
			rl.ctxLogger.Warn("empty stream from upstream, trying next target",
				"model", target.Model, "provider", target.Provider)
			return false
		}
		return true
	case actionResponse:
		result := rl.p.handleNonStreamingResponse(rl.gw, rl.w, rl.r, target, rl.opts.AgentName, rl.opts.SourceFormat, action, rl.opts.CacheKey, rl.opts.Cooldown)
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

	if util.IsContextLengthError(action.resp.StatusCode, string(action.body)) {
		return rl.handleContextLimitError(i, target, action)
	}

	shouldRetry, retryDelay := rl.handleUpstreamError(i, target, action)
	if !shouldRetry {
		gwErr := ParseProviderError(target.Provider, action.resp.StatusCode, action.body, nil)
		if rl.stream {
			writeGatewayStreamError(rl.w, action.resp.StatusCode, gwErr.Type, gwErr.Message)
		} else {
			writeGatewayError(rl.w, action.resp.StatusCode, gwErr.Type, gwErr.Message)
		}
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

// handleContextLimitError processes context-length exceeded errors with optional summarization.
func (rl *retryLoop) handleContextLimitError(i int, target routing.UpstreamTarget, action upstreamAction) retrySignal {
	rl.gw.Metrics.RecordContextLimitError(rl.opts.AgentName, target.Provider, target.Model)
	rl.ctxLogger.Warn("context length exceeded error from upstream",
		"status", action.resp.StatusCode,
		"provider", target.Provider,
		"model", target.Model)

	if !rl.gw.Config.Governance.AutoRetryOnContextLimitEnabled() {
		rl.ctxLogger.Info("auto_retry_on_context_limit disabled")
	} else if !rl.summarized {
		summarizedPayload, sumErr := rl.p.attemptContextLimitSummarization(
			rl.r.Context(), rl.ctxLogger, rl.gw, rl.originalPayload, action.body, rl.opts.AgentName, target.Provider, target.Model)
		if sumErr == nil && summarizedPayload != nil {
			rl.summarized = true
			rl.summarizedPayload = summarizedPayload
			rl.gw.Metrics.RecordSummarizationRetry(rl.opts.AgentName, target.Provider, target.Model)
			rl.ctxLogger.Info("context limit summarization succeeded, retrying with summarized payload")
			return retrySignalContinue
		}
		rl.ctxLogger.Warn("context limit summarization failed", "err", sumErr)
	} else {
		rl.ctxLogger.Warn("already attempted summarization")
	}

	gwErr := ParseProviderError(target.Provider, action.resp.StatusCode, action.body, nil)
	if rl.stream {
		writeGatewayStreamError(rl.w, action.resp.StatusCode, gwErr.Type, gwErr.Message)
	} else {
		writeGatewayError(rl.w, action.resp.StatusCode, gwErr.Type, gwErr.Message)
	}
	return retrySignalDone
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
		if err := rl.r.Context().Err(); err != nil {
			rl.ctxLogger.Debug("request context canceled, stopping failover sweep", "err", err)
			break retryLoop
		}
		if rl.opts.MaxRetries > 0 && rl.attempt >= rl.opts.MaxRetries {
			rl.ctxLogger.Warn("max retries reached", "attempt", rl.attempt, "max", rl.opts.MaxRetries)
			break retryLoop
		}

		var payloadToUse map[string]interface{}
		if rl.summarized && rl.summarizedPayload != nil {
			payloadToUse = rl.summarizedPayload
		} else {
			if !rl.copyPayload(workingPayload, i) {
				continue
			}
			payloadToUse = workingPayload
		}

		action := rl.prepareAndSend(i, target, payloadToUse)
		if err := rl.r.Context().Err(); err != nil {
			rl.ctxLogger.Debug("request context canceled during prepareAndSend, stopping failover sweep", "err", err)
			break retryLoop
		}
		if !rl.handleActionResult(i, target, action) {
			continue
		}
		return true
	}
	return false
}

// prepareAndSend wraps the proxy's prepareAndSend method.
func (rl *retryLoop) prepareAndSend(idx int, target routing.UpstreamTarget, payload map[string]interface{}) upstreamAction {
	return rl.p.prepareAndSend(rl.gw, rl.r, idx, rl.opts.Targets, target, payload, rl.opts.Cooldown, rl.opts.TokenCount, rl.opts.AgentName)
}

// handleUpstreamError wraps the proxy's handleUpstreamError method.
func (rl *retryLoop) handleUpstreamError(idx int, target routing.UpstreamTarget, action upstreamAction) (bool, time.Duration) {
	shouldRetry, delay := rl.p.handleUpstreamError(rl.gw, idx, rl.opts.Targets, target, rl.opts.Cooldown, rl.opts.AgentName, action)
	if rl.p.lastQuotaExhausted.Load() {
		rl.quotaExhausted = true
		rl.p.lastQuotaExhausted.Store(false)
	}
	return shouldRetry, delay
}

// Exhausted is called when all upstream targets have been exhausted without success.
// Writes an error response to the client. If quota exhaustion was detected during retries,
// returns error_kind=quota_exhausted; otherwise returns error_kind=provider_error.
func (rl *retryLoop) Exhausted() {
	rl.ctxLogger.Error("all upstream targets exhausted", "total", len(rl.opts.Targets), "attempts", rl.attempt)
	if rl.opts.AgentName != "" {
		rl.gw.Metrics.RecordExhausted(rl.opts.AgentName)
	}
	var errType ErrorType
	var message string
	if rl.quotaExhausted {
		errType = ErrorTypeQuotaExhausted
		message = "Quota exhausted on all upstream targets"
	} else {
		errType = ErrorTypeProvider
		message = "All upstream targets exhausted"
	}
	if rl.stream {
		writeGatewayStreamError(rl.w, http.StatusServiceUnavailable, errType, message)
	} else {
		writeGatewayError(rl.w, http.StatusServiceUnavailable, errType, message)
	}
}

// forwardToUpstream processes chat completion requests with retry logic across all targets.
// Creates a retryLoop, tracks in-flight metrics, and runs the retry loop. If retry loop
// creation fails, returns 500 Internal Server Error.
func (p *Proxy) forwardToUpstream(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, opts forwardOptions) {
	rl, err := newRetryLoop(p, gw, w, r, opts)
	if err != nil {
		writeGatewayError(w, http.StatusInternalServerError, ErrorTypeProvider, "Internal Server Error")
		return
	}

	if rl.Run() {
		return
	}

	rl.Exhausted()
}

// logRequestIfDebug logs request details at Debug level. Header values are
// intentionally NOT logged — CodeQL's go/clear-text-logging query traces all
// http.Header values as potentially sensitive (Authorization, Cookie, API keys).
// Only header_count is logged for debugging context without exposing values.
func logRequestIfDebug(ctx context.Context, logger *slog.Logger, req *http.Request, targetURL string, body []byte) {
	if !logger.Enabled(ctx, slog.LevelDebug) {
		return
	}
	logger.Debug("forwarding to upstream", "url", targetURL, "header_count", len(req.Header))
	if len(body) > 0 && len(body) < 1000 {
		logger.Debug("request body", "body", string(body))
	}
}

func handleUpstreamResponse(ctxLogger *slog.Logger, resp *http.Response, cancel context.CancelFunc) upstreamAction {
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(strings.ToLower(ct), "text/html") {
		ctxLogger.Warn("upstream returned HTML instead of API response, skipping target",
			"content_type", ct, "status", resp.StatusCode)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		cancel()
		return upstreamAction{kind: actionContinue}
	}
	isSSE := strings.Contains(strings.ToLower(ct), "text/event-stream")
	if isSSE {
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

	if action := p.checkPreDispatchGuards(gw, ctxLogger, target, tokenCount, agentName); action != nil {
		return *action
	}

	transformDeps := routing.TransformDeps{
		Logger:             gw.Logger,
		Providers:          gw.Providers,
		Config:             &gw.Config,
		ThoughtSigCache:    gw.ThoughtSigCache,
		ExtractContentText: gateway.ExtractContentText,
		Catalog:            gw.ModelCatalog,
		CountTokens:        gw.CountTokens,
		AgentName:          agentName,
	}
	transformedBody, _, err := routing.TransformRequestForUpstream(transformDeps, target.Provider, target.URL, payload, target.Model, target.MaxOutput, target.Format, target.ReasoningEffort)
	if err != nil {
		ctxLogger.Warn("failed to transform request, using original payload", "err", err)
		transformedBody, _ = json.Marshal(payload)
	}

	req, err := p.buildUpstreamRequest(gw, r.Context(), r.Method, target.URL, transformedBody, target.Provider, target.Model, target.Credential, r.Header)
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
		p.recordNetworkError(ctxLogger, gw, target, err, r, cooldownDuration)
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

	return handleUpstreamResponse(ctxLogger, resp, upstreamCancel)
}

// recordNetworkError records an upstream network error, distinguishing between
// true client disconnects (parent context canceled or deadline exceeded — no CB
// pollution) and real provider failures (connection refused, provider timeout on
// child context). When the parent context is healthy but the error is
// context-related, the provider timed out and RecordFailure is called.
func (p *Proxy) recordNetworkError(ctxLogger *slog.Logger, gw *gateway.NenyaGateway, target routing.UpstreamTarget, err error, r *http.Request, cooldownDuration time.Duration) {
	if util.IsContextCanceled(err) {
		// Provider timeout (DeadlineExceeded on child context) IS a provider failure.
		// Client disconnect (parent context terminated) is NOT.
		if r.Context().Err() == nil {
			// Parent is healthy → child timed out → provider failure
			ctxLogger.Warn("target network error (provider timeout)", "err", err)
			gw.AgentState.RecordFailure(target, cooldownDuration)
			return
		}
		// Parent is also terminated → client disconnect, skip CB pollution
		ctxLogger.Debug("request canceled (client disconnect), releasing half-open CB slot", "err", err)
		gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
		return
	}
	ctxLogger.Warn("target network error", "err", err)
	gw.AgentState.RecordFailure(target, cooldownDuration)
}

func (p *Proxy) checkPreDispatchGuards(gw *gateway.NenyaGateway, ctxLogger *slog.Logger, target routing.UpstreamTarget, tokenCount int, agentName string) *upstreamAction {
	if !gw.RateLimiter.Check(target.URL, tokenCount) {
		gw.Metrics.RecordRateLimitRejected(infra.ExtractHost(target.URL))
		ctxLogger.Warn("target skipped: rate limit exceeded")
		return ptrAction(upstreamAction{kind: actionContinue})
	}

	if gw.Config.Governance.MaxCostPerRequest > 0 {
		dm, ok := gw.ModelCatalog.Lookup(target.Model)
		if ok && dm.Pricing != nil && !dm.Pricing.IsZero() {
			estCost := dm.Pricing.CalculateCost(int64(tokenCount), int64(target.MaxOutput))
			if estCost > gw.Config.Governance.MaxCostPerRequest {
				gw.Metrics.RecordCostLimitRejected(target.Model)
				ctxLogger.Warn("target skipped: estimated cost exceeds max_cost_per_request",
					"estimated_cost_usd", estCost, "max_cost_usd", gw.Config.Governance.MaxCostPerRequest)
				return ptrAction(upstreamAction{kind: actionContinue})
			}
		}
	}

	agent, ok := gw.Config.Agents[agentName]
	if ok && agent.BudgetLimitUSD > 0 && gw.BillingTracker != nil {
		account := gw.BillingTracker.GetTotalSpend(target.Provider, target.AccountName)
		if account >= agent.BudgetLimitUSD {
			gw.Metrics.RecordBudgetLimitRejected(target.Model)
			ctxLogger.Warn("target skipped: agent budget exhausted",
				"agent", agentName, "spent_usd", account, "budget_usd", agent.BudgetLimitUSD)
			return ptrAction(upstreamAction{kind: actionContinue})
		}
	}

	if target.CoolKey != "" && !gw.AgentState.CB.Allow(target.CoolKey) {
		ctxLogger.Warn("target skipped: circuit breaker open")
		return ptrAction(upstreamAction{kind: actionContinue})
	}

	return nil
}

func ptrAction(a upstreamAction) *upstreamAction {
	return &a
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

func handleRetryableError429(logger *slog.Logger, errorBody []byte, action upstreamAction, cooldownDuration time.Duration, target routing.UpstreamTarget, agentName string, gw *gateway.NenyaGateway) (time.Duration, bool) {
	isQuota := false
	effectiveCooldown := cooldownDuration
	if action.resp.StatusCode == http.StatusTooManyRequests && len(errorBody) > 0 {
		if quotaCD := parseQuotaExhaustion(errorBody, logger); quotaCD > 0 {
			isQuota = true
			if quotaCD > cooldownDuration {
				effectiveCooldown = quotaCD
				logger.Info("quota exhaustion detected, extending cooldown", "cooldown_s", effectiveCooldown.Seconds())
			}
		}
	}

	if action.resp.StatusCode == http.StatusTooManyRequests {
		gw.AgentState.ActivateCooldown(target, effectiveCooldown)
		gw.Metrics.RecordCooldown(agentName, target.Provider, target.Model)
		gw.AgentState.RecordFailureWithStatus(target, action.resp.StatusCode, string(errorBody))
		return parseRetryDelay(action.resp.Header, errorBody), isQuota
	}

	gw.AgentState.RecordFailureWithStatus(target, action.resp.StatusCode, string(errorBody))
	return 0, false
}

func handleAdapterRetryableError(ctxLogger *slog.Logger, target routing.UpstreamTarget, action upstreamAction, cooldownDuration time.Duration, gw *gateway.NenyaGateway) (bool, bool) {
	a := adapter.ForProvider(target.Provider)
	errClass := a.NormalizeError(action.resp.StatusCode, action.body)
	if (errClass == adapter.ErrorRetryable || errClass == adapter.ErrorQuotaExhausted) && action.resp.StatusCode >= 400 && action.resp.StatusCode < 500 {
		ctxLogger.Warn("adapter classified client error as retryable, trying next target", "error_class", errClass)
		gw.AgentState.RecordFailureWithStatus(target, action.resp.StatusCode, string(action.body))
		return true, errClass == adapter.ErrorQuotaExhausted
	}
	return false, false
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
		delay, isQuota := handleRetryableError429(ctxLogger, errorBody, action, cooldownDuration, target, agentName, gw)
		if isQuota {
			p.lastQuotaExhausted.Store(true)
		}
		return true, delay
	}

	if isRetryableClientErrorForProvider(action.resp.StatusCode, errorBody, target.Provider) && len(targets) > 1 {
		logBody := redactForLog(string(errorBody), gw)
		ctxLogger.Warn("retryable client error from upstream, trying next target", "body", logBody)
		gw.AgentState.RecordFailureWithStatus(target, action.resp.StatusCode, string(errorBody))
		return true, 0
	}

	if retryable, isQuota := handleAdapterRetryableError(ctxLogger, target, action, cooldownDuration, gw); retryable {
		if isQuota {
			p.lastQuotaExhausted.Store(true)
		}
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

// attemptContextLimitSummarization attempts to summarize the request messages
// after a context-length error. It parses the original payload, extracts messages,
// sends them to the configured summarization engine with the provided context,
// and returns a summarized payload map on success.
func (p *Proxy) attemptContextLimitSummarization(ctx context.Context, ctxLogger *slog.Logger, gw *gateway.NenyaGateway, originalPayload []byte, errorBody []byte, agentName, providerName, modelName string) (map[string]interface{}, error) {
	if len(gw.Config.Bouncer.Engine.ResolvedTargets) == 0 {
		return nil, fmt.Errorf("engine chain not configured")
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(originalPayload, &payload); err != nil {
		ctxLogger.Warn("failed to unmarshal original payload for summarization", "err", err)
		return nil, fmt.Errorf("failed to unmarshal original payload: %w", err)
	}

	messagesRaw, ok := payload["messages"]
	if !ok || messagesRaw == nil {
		return nil, fmt.Errorf("no messages in payload")
	}

	messages, ok := messagesRaw.([]interface{})
	if !ok || len(messages) == 0 {
		return nil, fmt.Errorf("messages is not a valid array or is empty")
	}

	summarized, err := p.summarizeMessages(ctx, gw, messages, agentName, providerName, modelName)
	if err != nil {
		return nil, err
	}

	newPayload := make(map[string]interface{}, len(payload))
	for k, v := range payload {
		newPayload[k] = v
	}
	newPayload["messages"] = summarized
	return newPayload, nil
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

// parseQuotaExhaustion extracts quota cooldown duration from error responses.
// It checks for ZAI-specific error codes (1308, 1310) with Unix millisecond
// timestamps in the message, and falls back to generic quota pattern matching.
// Returns 0 if no quota information is found.
func parseQuotaExhaustion(body []byte, logger *slog.Logger) time.Duration {
	if len(body) == 0 {
		return 0
	}

	var errResp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if json.Unmarshal(body, &errResp) != nil {
		return parseGenericQuotaPatterns(body)
	}

	if errResp.Error.Code == "" {
		return parseGenericQuotaPatterns(body)
	}

	switch errResp.Error.Code {
	case "1308":
		return parseZaiQuota1308(errResp.Error.Message)
	case "1310":
		return parseZaiQuota1310(errResp.Error.Message)
	default:
		return parseGenericQuotaPatterns(body)
	}
}

func parseZaiQuota1308(message string) time.Duration {
	ts := extractUnixTimestampMs(message)
	if ts <= 0 {
		return zaiCode1308FallbackCooldown
	}

	dur := time.Until(time.UnixMilli(ts))
	if dur > 0 {
		return dur
	}
	return zaiCode1308FallbackCooldown
}

func parseZaiQuota1310(message string) time.Duration {
	ts := extractUnixTimestampMs(message)
	if ts <= 0 {
		return zaiCode1310FallbackCooldown
	}

	dur := time.Until(time.UnixMilli(ts))
	if dur <= 0 {
		return zaiCode1310FallbackCooldown
	}

	if dur < zaiCode1310MinCooldown {
		return zaiCode1310MinCooldown
	}
	if dur > maxQuotaCooldown {
		return maxQuotaCooldown
	}
	return dur
}

func parseGenericQuotaPatterns(body []byte) time.Duration {
	lower := strings.ToLower(string(body))

	if strings.Contains(lower, "per 86400s") || strings.Contains(lower, "perday") {
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

// extractUnixTimestampMs searches for and extracts the first 13+ digit
// number from the message, returning it as a millisecond-precision Unix timestamp.
// Only timestamps between 2023-01-01 (1672531200000) and 9999-12-31 23:59:59 UTC
// (410244479903999) are accepted — numbers outside this range are rejected.
// Returns 0 if no 13+ digit number is found, or if the number is outside the valid range.
// This function is stateless and safe for concurrent use.
func extractUnixTimestampMs(msg string) int64 {
	const (
		minTimestamp = int64(1672531200000)
		maxTimestamp = int64(410244479903999)
	)

	for i := 0; i < len(msg); i++ {
		if msg[i] < '0' || msg[i] > '9' {
			continue
		}

		j := i
		for j < len(msg) && msg[j] >= '0' && msg[j] <= '9' {
			j++
		}

		if j-i < 13 {
			i = j
			continue
		}

		ts := parseDigitsToTimestamp(msg[i:j], minTimestamp, maxTimestamp)
		if ts > 0 {
			return ts
		}
		i = j
	}
	return 0
}

// parseDigitsToTimestamp parses a digit string into an int64 timestamp,
// applying overflow protection and range validation.
func parseDigitsToTimestamp(digits string, minTimestamp, maxTimestamp int64) int64 {
	var ts int64
	for k := range len(digits) {
		digit := int64(digits[k] - '0')
		if ts > (math.MaxInt64-digit)/10 {
			return 0
		}
		ts = ts*10 + digit
	}

	if ts < minTimestamp || ts > maxTimestamp {
		return 0
	}
	return ts
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
	529, // Provider Overloaded (non-standard, used by xAI and some LLM gateways)
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

var openrouterRetryablePatterns = []string{
	"insufficient_quota",
	"insufficient balance",
	"no available provider",
	"no available model",
	"capacity exceeded",
	"capacity_limit",
	"provider overloaded",
	"free tier rate limit",
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
	{name: "openrouter", patterns: openrouterRetryablePatterns},
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
