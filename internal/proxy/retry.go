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
	"sync"
	"time"

	"github.com/nenya/config"
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

const (
	// concurrencyRetryBase is the fixed wait before retrying after an
	// upstream concurrency-limit rejection (NENYA-69, e.g. ZAI 1302).
	concurrencyRetryBase = 200 * time.Millisecond
	// concurrencyRetryJitter is added to concurrencyRetryBase to desynchronize
	// concurrent waiters.
	concurrencyRetryJitter = 100 * time.Millisecond
)

// concurrencyRetryDelay returns the wait applied after an upstream
// concurrency-limit rejection.
func concurrencyRetryDelay() time.Duration {
	return concurrencyRetryBase + time.Duration(rand.Int63n(int64(concurrencyRetryJitter)))
}

// upstreamAction holds the result of a single upstream HTTP request attempt.
// The kind field distinguishes between streaming, response, and error outcomes.
// release frees the per-model concurrency slot held for the round-trip; it is
// invoked at every terminal handling point and is idempotent.
type upstreamAction struct {
	kind    int
	resp    *http.Response
	body    []byte
	cancel  context.CancelFunc
	release func()
	// retryable marks an actionContinue that carried a transient local
	// failure (network error) eligible for a same-target retry when no
	// later target remains. Guard/skip actions leave it false.
	retryable bool
}

// releaseSlot frees the concurrency slot held by this action, if any. The
// release function is Once-wrapped at acquire time, so double invocation is
// safe.
func (a upstreamAction) releaseSlot() {
	if a.release != nil {
		a.release()
	}
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
		ctx, gw.ClientFor,
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
		resp, fetchErr := gw.ClientFor(providerName).Do(upstreamReq)
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
	Targets    []routing.UpstreamTarget
	Payload    map[string]any
	Stream     bool
	Cooldown   time.Duration
	TokenCount int
	AgentName  string
	// Agent is the resolved agent config for AgentName (zero value for
	// direct model routes), resolved once in resolveRouting.
	Agent        config.AgentConfig
	MaxRetries   int
	CacheKey     string
	KeyRef       string
	SourceFormat string
	ApiKey       *config.ApiKey
	// Canary carries the per-request tripwire token (empty when the
	// guard is disabled).
	Canary pipeline.CanarResult
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
	// lastStreamErr holds the final transport-level stream read error observed
	// across targets (distinct from an empty stream). Its detail is logged, not
	// echoed to the client after Exhausted(), to avoid leaking internal
	// network topology.
	lastStreamErr error
	// rateLimitedPairs tracks provider+account pairs that returned 429 during
	// this failover round (NENYA-41): later targets on the same pair are
	// skipped while a different pair remains eligible.
	// lastFailReason classifies the most recent target failure for the
	// sticky_provider failover gate (NENYA-18); reset before each dispatch.
	lastFailReason failReason
	// keyBudgetCharged ensures the per-key daily token budget is charged
	// once per request, not per failover attempt (NENYA-20).
	keyBudgetCharged bool
	rateLimitedPairs map[string]bool
	// paramRejectRetried ensures the strip-and-retry safety net (NENYA-32)
	// fires at most once per request.
	paramRejectRetried bool
	// sameTargetRetries counts re-attempts of the last target performed
	// because a retryable failure had no later target to fail over to. It
	// is bounded by maxSameTargetRetries so a persistently retryable
	// failure (including ones that intentionally skip circuit-breaker
	// accounting, e.g. concurrency limits) cannot loop forever.
	sameTargetRetries int
	// dispatches counts every call to prepareAndSend, including guard skips
	// that dispatch nothing. It complements `attempt` (error actions only)
	// in the exhaustion log.
	dispatches int
	// waited records that handleActionError already slept (or deliberately
	// skipped the sleep for an immediate corrective retry) before Run decides
	// whether to repeat the same target, so the backoff is not applied twice.
	waited bool
	// lastRetryable4xx snapshots the most recent retryable 4xx upstream
	// error so Exhausted can relay the real client-class status instead of
	// a generic 503 when the retry budget runs out.
	lastRetryable4xx *upstreamErrorSnapshot
}

// upstreamErrorSnapshot captures the parts of a retryable upstream error
// needed to re-render it to the client after the retry budget is exhausted.
type upstreamErrorSnapshot struct {
	provider string
	status   int
	body     []byte
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

// actionOutcome classifies how the retry loop should proceed after a
// dispatched target.
type actionOutcome struct {
	// handled marks a terminal outcome: a response (or terminal error) was
	// written to the client and the loop must stop.
	handled bool
	// retryable marks a transient failure that may be re-attempted on the
	// same target when no later target remains. Run decides whether to
	// advance (a later target exists) or repeat (last target).
	retryable bool
	// status is the upstream HTTP status for error outcomes (0 otherwise),
	// used to keep 413 failover-only.
	status int
}

// handleActionResult dispatches on action.kind and reports whether the
// request is handled and, when not, whether the failure is retryable.
func (rl *retryLoop) handleActionResult(i int, target routing.UpstreamTarget, action upstreamAction) actionOutcome {
	defer action.releaseSlot()
	// Any outcome other than a classified upstream error invalidates the
	// retryable-4xx snapshot: the exhaustion relay must reflect the last
	// failure. handleUpstreamError re-snapshots when this attempt is itself a
	// retryable 4xx.
	if action.kind != actionError {
		rl.lastRetryable4xx = nil
	}
	switch action.kind {
	case actionContinue:
		return rl.handleResumeAction(action)
	case actionError:
		return rl.handleErrorActionOutcome(i, target, action)
	case actionStream:
		return rl.handleStreamAction(i, target, action)
	default:
		return rl.handleNonStreamingActionOutcome(i, target, action)
	}
}

// handleErrorActionOutcome classifies an upstream error action.
// retrySignalRetry is eligible for a same-target repeat; retrySignalContinue
// only advances.
func (rl *retryLoop) handleErrorActionOutcome(i int, target routing.UpstreamTarget, action upstreamAction) actionOutcome {
	if action.resp.StatusCode >= http.StatusInternalServerError {
		rl.lastFailReason = failReason5xx
	} else {
		rl.lastFailReason = failReason4xx
	}
	status := action.resp.StatusCode
	switch rl.handleActionError(i, target, action) {
	case retrySignalDone:
		return actionOutcome{handled: true, status: status}
	case retrySignalRetry:
		return actionOutcome{retryable: true, status: status}
	}
	return actionOutcome{status: status}
}

// handleResumeAction handles a local dispatch outcome (network failure or
// guard skip): a network failure is retryable. Backoff is applied by Run
// immediately before a same-target repeat, so a plain failover to a healthy
// later target stays immediate.
func (rl *retryLoop) handleResumeAction(action upstreamAction) actionOutcome {
	rl.lastFailReason = failReasonDispatch
	return actionOutcome{retryable: action.retryable}
}

// handleStreamAction maps a streaming attempt result to a loop outcome.
func (rl *retryLoop) handleStreamAction(i int, target routing.UpstreamTarget, action upstreamAction) actionOutcome {
	result := rl.p.streamResponse(streamResponseOpts{
		gw:           rl.gw,
		w:            rl.w,
		r:            rl.r,
		target:       target,
		agentName:    rl.opts.AgentName,
		sourceFormat: rl.opts.SourceFormat,
		cacheKey:     rl.opts.CacheKey,
		cooldown:     rl.opts.Cooldown,
		payload:      rl.opts.Payload,
		targets:      rl.opts.Targets,
		idx:          i,
		tokenCount:   rl.opts.TokenCount,
		apiKey:       rl.opts.ApiKey,
		canary:       rl.opts.Canary,
	}, action)
	if result.terminal {
		// Response fully written (content-policy block): stop, never append
		// another target's output to the committed bytes.
		return actionOutcome{handled: true}
	}
	if result.empty {
		rl.lastFailReason = failReasonStream
		rl.ctxLogger.Warn("empty stream from upstream, trying next target",
			"model", target.Model, "provider", target.Provider)
		return actionOutcome{retryable: true}
	}
	if result.err != nil {
		// Transport-level failure before any stream bytes: fail over; on the
		// last target Exhausted() surfaces the error.
		rl.lastFailReason = failReasonStream
		rl.lastStreamErr = result.err
		rl.ctxLogger.Warn("stream read error from upstream, trying next target",
			"err", result.err, "model", target.Model, "provider", target.Provider)
		return actionOutcome{retryable: true}
	}
	return actionOutcome{handled: true}
}

// handleNonStreamingActionOutcome maps a non-streaming attempt result to a
// loop outcome (the actionResponse case plus the defensive default).
func (rl *retryLoop) handleNonStreamingActionOutcome(i int, target routing.UpstreamTarget, action upstreamAction) actionOutcome {
	if action.kind != actionResponse {
		return actionOutcome{}
	}
	result := rl.p.handleNonStreamingResponse(rl.gw, rl.w, rl.r, target, rl.opts.AgentName, rl.opts.SourceFormat, action, rl.opts.CacheKey, rl.opts.Cooldown, rl.opts.Canary)
	if result.terminal {
		// 403 already written (exfil block): stop the loop.
		return actionOutcome{handled: true}
	}
	if result.empty {
		rl.ctxLogger.Warn("empty non-streaming response from upstream, trying next target",
			"model", target.Model, "provider", target.Provider)
		return actionOutcome{retryable: true}
	}
	return actionOutcome{handled: true}
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
		p:                p,
		gw:               gw,
		w:                w,
		r:                r,
		opts:             opts,
		ctxLogger:        ctxLogger,
		originalPayload:  originalPayload,
		stream:           opts.Stream,
		rateLimitedPairs: make(map[string]bool),
	}, nil
}

// accountPair returns the round-scoped dedup key for a target: provider plus
// account when multi-account, provider alone otherwise.
func (rl *retryLoop) accountPair(target routing.UpstreamTarget) string {
	if target.AccountName != "" {
		return target.Provider + "/" + target.AccountName
	}
	return target.Provider
}

// markRateLimitedPair records that a target's provider+account pair returned
// 429 during this failover round.
func (rl *retryLoop) markRateLimitedPair(target routing.UpstreamTarget) {
	rl.rateLimitedPairs[rl.accountPair(target)] = true
}

// shouldSkipRateLimitedPair decides whether a target is benched for the
// remainder of the round: its provider+account pair already returned 429 AND
// a different un-limited pair remains later in the chain (a single-credential
// chain never skips, preserving backoff-retry semantics).
func (rl *retryLoop) shouldSkipRateLimitedPair(i int, target routing.UpstreamTarget) bool {
	if !rl.rateLimitedPairs[rl.accountPair(target)] {
		return false
	}
	return rl.hasOtherEligiblePair(i)
}

// hasOtherEligiblePair reports whether any target after index i belongs to a
// provider+account pair that has not already been rate-limited this round.
// The target at index i itself is excluded from the scan, so a sole
// single-credential chain reports false (preserving its backoff semantics).
func (rl *retryLoop) hasOtherEligiblePair(i int) bool {
	if i < 0 || i >= len(rl.opts.Targets) {
		return false
	}
	for j := i + 1; j < len(rl.opts.Targets); j++ {
		if pair := rl.accountPair(rl.opts.Targets[j]); !rl.rateLimitedPairs[pair] {
			return true
		}
	}
	return false
}

// retrySignal controls the outer loop from action handlers.
type retrySignal int

const (
	// retrySignalContinue advances to the next target (failover).
	retrySignalContinue retrySignal = iota
	// retrySignalRetry is a transient failure eligible for a same-target
	// re-attempt when no later target remains.
	retrySignalRetry
	retrySignalBreak
	retrySignalDone
)

// handleActionError processes an upstream error action, applies backoff, and returns a loop signal.
func (rl *retryLoop) handleActionError(i int, target routing.UpstreamTarget, action upstreamAction) retrySignal {
	rl.attempt++
	// Any previously remembered retryable 4xx is stale the moment a new
	// upstream error is classified; handleUpstreamError re-snapshots below
	// when this failure is itself a retryable 4xx.
	rl.lastRetryable4xx = nil
	action.body, _ = io.ReadAll(io.LimitReader(action.resp.Body, pipeline.MaxErrorBodyBytes))
	_ = action.resp.Body.Close()
	// The upstream response is fully consumed; release its request context on
	// every path so retries do not abandon in-flight upstream contexts.
	// CancelFunc is idempotent, so the later cancels are harmless.
	if action.cancel != nil {
		action.cancel()
	}

	// Connection-scoped (client cancel/disconnect, NENYA-42): the client is
	// gone. Surface nothing, mutate no resilience state, retry nothing.
	if err := rl.r.Context().Err(); err != nil {
		action.cancel()
		// A consumed half-open probe slot would otherwise leak.
		rl.gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
		rl.ctxLogger.Info("client context canceled during upstream error handling",
			"model", target.Model, "provider", target.Provider, "error_message", err.Error())
		return retrySignalDone
	}

	// Attempted-set (NENYA-41): a 429 rate/quota failure benches the target's
	// provider+account pair for the remainder of this failover round — the
	// same credential would just 429 again. Other pairs are still eligible.
	if action.resp.StatusCode == http.StatusTooManyRequests {
		rl.markRateLimitedPair(target)
	}

	if util.IsContextLengthError(action.resp.StatusCode, string(action.body)) {
		return rl.handleContextLimitError(i, target, action)
	}

	// Param-reject safety net (NENYA-32): a 400 that names a parameter the
	// compat table knows how to drop gets one strip-and-retry chance.
	// Context-length handling above keeps precedence; when the safety net
	// fires it re-enters the loop immediately (no backoff) with the
	// stripped payload, bypassing request-scoped rules for that attempt.
	if action.resp.StatusCode == http.StatusBadRequest && rl.maybeStripAndRetryParamReject(i, target, action) {
		rl.waited = true // corrective retry: no backoff, Run must not sleep
		// The retry re-dispatches and consumes a fresh probe; release this
		// attempt's probe so the corrective retry does not leak a slot.
		rl.gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
		return retrySignalRetry
	}

	// Request-scoped via provider config (NENYA-42): the client's payload is
	// at fault. Skip cooldown/rotation state and surface the upstream error
	// directly to the client instead of sweeping the remaining targets.
	// Note: context-length handling above wins over rules — an operator rule
	// cannot disable the summarization retry for ctx-limit-shaped errors.
	if matched := rl.p.matchRequestScopedError(rl.gw, target.Provider, action.resp.StatusCode, action.body); matched != nil {
		gwErr := ParseProviderError(target.Provider, action.resp.StatusCode, action.body, nil)
		rl.ctxLogger.Warn("request-scoped error from provider, failing without rotation",
			"model", target.Model, "provider", target.Provider, "status", action.resp.StatusCode,
			"rule_pattern", matched.MessagePattern)
		action.cancel()
		// No circuit outcome is recorded for a client-fault error; release the
		// probe slot consumed by the pre-dispatch guard.
		rl.gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
		rl.writeUpstreamErrorToClient(action.resp.StatusCode, gwErr)
		return retrySignalDone
	}

	signal, retryDelay := rl.handleUpstreamError(i, target, action)
	rl.rememberRetryable4xx(target, action, signal)
	if signal == retrySignalDone {
		gwErr := ParseProviderError(target.Provider, action.resp.StatusCode, action.body, nil)
		rl.writeUpstreamErrorToClient(action.resp.StatusCode, gwErr)
		return retrySignalDone
	}
	// max_retries bounds the retry budget. handleActionError increments
	// attempt only for upstream error actions, and Run's top-of-loop check
	// (attempt > Max) lets same-target/local repeats consume the remainder;
	// together, Max permits Max retries beyond the initial attempt.
	if rl.opts.MaxRetries > 0 && rl.attempt > rl.opts.MaxRetries {
		return retrySignalBreak
	}
	// Concurrency saturation follows its own short fixed pacing: honor the
	// delay handleUpstreamError derived, then mark the wait so Run does not
	// add the generic backoff on top.
	if signal == retrySignalRetry && adapter.ForProvider(target.Provider).NormalizeError(action.resp.StatusCode, action.body) == adapter.ErrorConcurrencyLimited {
		if retryDelay > 0 {
			rl.ctxLogger.Info("retrying with concurrency delay",
				"model", target.Model, "delay_ms", retryDelay.Milliseconds())
			waitWithCancel(rl.r.Context(), retryDelay)
		}
		rl.waited = true
		return signal
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
	rl.waited = true
	return signal
}

// rememberRetryable4xx snapshots a retryable 4xx so a later exhaustion can
// relay the real client-class status instead of a generic 503. 429 is excluded
// (its quota semantics keep the 503 quota_exhausted flow). Any other failure
// clears a previously remembered snapshot: the exhaustion relay must reflect
// the *last* failure, not a stale earlier target's 4xx.
func (rl *retryLoop) rememberRetryable4xx(target routing.UpstreamTarget, action upstreamAction, signal retrySignal) {
	if signal != retrySignalRetry || action.resp.StatusCode < 400 || action.resp.StatusCode >= 500 ||
		action.resp.StatusCode == http.StatusTooManyRequests {
		rl.lastRetryable4xx = nil
		return
	}
	// A 4xx-class quota exhaustion (e.g. ZAI 1308/1310 on 403) is not a
	// client error: it must keep the 503 quota_exhausted contract.
	if rl.quotaExhausted {
		rl.lastRetryable4xx = nil
		return
	}
	rl.lastRetryable4xx = &upstreamErrorSnapshot{
		provider: target.Provider,
		status:   action.resp.StatusCode,
		body:     append([]byte(nil), action.body...),
	}
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
			rl.r.Context(), rl.ctxLogger, rl.gw, rl.originalPayload, action.body, rl.opts.AgentName, target.Provider, target.Model, rl.opts.Canary)
		if sumErr == nil && summarizedPayload != nil {
			rl.summarized = true
			rl.summarizedPayload = summarizedPayload
			rl.gw.Metrics.RecordSummarizationRetry(rl.opts.AgentName, target.Provider, target.Model)
			rl.ctxLogger.Info("context limit summarization succeeded, retrying with summarized payload")
			rl.lastFailReason = failReasonSummarized
			// The summarized retry re-dispatches and consumes a fresh probe.
			rl.gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
			return retrySignalRetry
		}
		rl.ctxLogger.Warn("context limit summarization failed", "err", sumErr)
	} else {
		rl.ctxLogger.Warn("already attempted summarization")
	}

	gwErr := ParseProviderError(target.Provider, action.resp.StatusCode, action.body, nil)
	rl.gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
	rl.writeUpstreamErrorToClient(action.resp.StatusCode, gwErr)
	return retrySignalDone
}

// maybeStripAndRetryParamReject implements the strip-and-retry safety net
// for upstream parameter rejections (NENYA-32): when a 400 error body
// names a well-known chat parameter that is present in the payload, the
// gateway strips it, records the strip, and queues one retry. The
// proactive param-compat table already removes table-known parameters
// pre-dispatch, so the net exists for rejections the table did not
// anticipate — hence the gate here is payload presence, not table
// membership. Returns true when the safety net fired (stripped payload
// queued, re-enter the loop immediately) and false when normal error
// handling should proceed (retry disabled, safety net already spent, no
// recognizable parameter in the body, or nothing present to strip).
func (rl *retryLoop) maybeStripAndRetryParamReject(i int, target routing.UpstreamTarget, action upstreamAction) bool {
	if rl.paramRejectRetried || !rl.gw.Config.Governance.AutoRetryOnParamRejectEnabled() {
		return false
	}
	params := extractRejectedParamNames(string(action.body))
	if len(params) == 0 {
		return false
	}

	// The retry re-dispatches from the pristine original payload, so the
	// strip must not compound with prior per-target transforms.
	var payload map[string]interface{}
	if rl.summarized && rl.summarizedPayload != nil {
		cp := make(map[string]interface{}, len(rl.summarizedPayload))
		for k, v := range rl.summarizedPayload {
			cp[k] = v
		}
		payload = cp
	} else {
		cp := make(map[string]interface{}, 16)
		if !rl.copyPayload(cp, i) {
			return false
		}
		payload = cp
	}

	// Only strip parameters the payload actually carries; an error body
	// can mention a parameter the client never sent.
	var strippable []string
	for _, p := range params {
		if _, ok := payload[p]; ok {
			strippable = append(strippable, p)
		}
	}
	if len(strippable) == 0 {
		return false
	}
	routing.StripParams(payload, strippable)

	for _, param := range strippable {
		rl.gw.Metrics.RecordParamRejectStrip(rl.opts.AgentName, target.Provider, target.Model, param, "retry")
	}
	rl.gw.Metrics.RecordParamRejectRetry(rl.opts.AgentName, target.Provider, target.Model)
	rl.paramRejectRetried = true

	// Re-enter the loop with the stripped payload in the summarized slot:
	// both flags matter — the slot holds the corrective payload and the
	// summarized flag is what the payload-selection gate checks. No
	// further param retries can occur (paramRejectRetried latches).
	rl.summarized = true
	rl.summarizedPayload = payload
	rl.lastFailReason = failReasonSummarized
	rl.ctxLogger.Info("upstream rejected known parameter, stripping and retrying once",
		"provider", target.Provider, "model", target.Model, "params", strings.Join(strippable, ","))
	return true
}

// extractRejectedParamNames scans an upstream error body for names of the
// well-known chat-completion parameters it complains about. Deliberately
// conservative: it only recognizes the top-level OpenAI parameters the
// compat table can drop, so arbitrary body text never yields matches.
func extractRejectedParamNames(body string) []string {
	lower := strings.ToLower(body)
	wellKnown := []string{
		"temperature", "top_p", "top_k", "presence_penalty",
		"frequency_penalty", "candidate_count", "reasoning_effort",
		"tool_choice", "stream_options", "logit_bias", "logprobs",
	}
	var found []string
	for _, p := range wellKnown {
		if strings.Contains(lower, p) {
			found = append(found, p)
		}
	}
	return found
}

// failReason classifies why the loop is about to move past a target, so
// the sticky_provider policy can gate failover (NENYA-18).
type failReason int

const (
	// failReasonNone is the zero value: no failover decision recorded.
	failReasonNone failReason = iota
	// failReasonSummarized: a context-limit summarization retry is
	// queued; both sticky modes allow the sweep to continue for it.
	failReasonSummarized
	// failReason5xx: an explicit server-side (>=500) upstream status.
	failReason5xx
	// failReason4xx: an upstream 4xx-class error (retryable by config).
	failReason4xx
	// failReasonDispatch: local dispatch/transport failure before any
	// upstream response.
	failReasonDispatch
	// failReasonStream: stream-level failure (empty, stalled, read error).
	failReasonStream
)

// enforceBudgets charges and checks the per-key daily token budget (once
// per request) and the per-provider daily budget (per target attempt).
// Returns denied=true with the denial reason ("key_budget" /
// "provider_budget"); the caller writes the typed 429 and ends the request.
func (rl *retryLoop) enforceBudgets(target routing.UpstreamTarget) (bool, string) {
	if rl.opts.TokenCount <= 0 {
		return false, ""
	}

	// Key daily budget: reserved once per request (non-refundable
	// estimate — failed dispatches consume the reservation).
	if !rl.keyBudgetCharged {
		rl.keyBudgetCharged = true
		if key := rl.opts.ApiKey; key != nil && key.TokenBudgetDaily > 0 {
			if !rl.gw.KeyUsage.ChargeKeyTokens(key.Name, key.TokenBudgetDaily, rl.opts.TokenCount) {
				return true, "key_budget"
			}
		}
	}

	// Provider daily budget with per-key priority tier.
	prov := rl.gw.Providers[target.Provider]
	if prov == nil || prov.TokenBudgetDaily <= 0 {
		return false, ""
	}
	tier := "always"
	if key := rl.opts.ApiKey; key != nil && key.BudgetTier != "" {
		tier = key.BudgetTier
	}
	if !rl.gw.KeyUsage.AllowProviderTokens(target.Provider, tier, prov.TokenBudgetDaily, rl.opts.TokenCount) {
		return true, "provider_budget"
	}
	return false, ""
}

// stickyAllowsFailover applies the agent's sticky_provider policy to a
// failover decision (NENYA-18). strict blocks every failover except a
// queued summarization retry; lenient allows 5xx, targets whose circuit
// was open when dispatched (Cooling), or summarization.
func (rl *retryLoop) stickyAllowsFailover(reason failReason, target routing.UpstreamTarget) bool {
	switch rl.opts.Agent.StickyProvider {
	case "strict":
		return reason == failReasonSummarized
	case "lenient":
		switch reason {
		case failReasonSummarized, failReason5xx:
			return true
		case failReasonDispatch, failReasonStream:
			return target.Cooling
		default:
			return false
		}
	default:
		return true
	}
}

// Run executes the retry loop. It returns true when the request has been fully
// handled (streaming or non-streaming success, or a terminal HTTP error response
// written to the client). It returns false when all targets are exhausted
// without sending a complete response, so the caller should respond with 503.
//
// The sweep is forward-only: a failure advances to the next target. When a
// retryable failure lands on the last target, the same target is re-attempted
// (bounded by maxSameTargetRetries) instead of exhausting with no retry.
func (rl *retryLoop) Run() bool {
	defer rl.trackInFlight()()
	workingPayload := make(map[string]interface{}, 16)
	for i := 0; i < len(rl.opts.Targets); i++ {
		target := rl.opts.Targets[i]
		if err := rl.r.Context().Err(); err != nil {
			rl.ctxLogger.Debug("request context canceled, stopping failover sweep", "err", err)
			break
		}
		if rl.opts.MaxRetries > 0 && rl.attempt > rl.opts.MaxRetries {
			rl.ctxLogger.Warn("max retries reached", "attempt", rl.attempt, "max", rl.opts.MaxRetries)
			break
		}

		// Attempted-set (NENYA-41): skip a target whose provider+account pair
		// already 429'd this round — but only while a different pair remains
		// later in the chain, so a single-credential chain keeps its
		// backoff-retry semantics.
		if rl.shouldSkipRateLimitedPair(i, target) {
			rl.ctxLogger.Info("skipping rate-limited account for remaining round",
				"model", target.Model, "provider", target.Provider, "account", target.AccountName)
			continue
		}

		// Per-key/per-provider token budgets (NENYA-20): charged once per
		// request for the key's daily budget, per dispatch attempt for the
		// provider's daily budget. Denials write a typed 429 and end the
		// request.
		if denied, reason := rl.enforceBudgets(target); denied {
			rl.gw.Metrics.IncAuthDenials(rl.opts.AgentName, reason)
			rl.ctxLogger.Warn("token budget exhausted, rejecting request",
				"reason", reason, "model", target.Model, "provider", target.Provider,
				"token_count", rl.opts.TokenCount)
			writeStructuredError(rl.w, http.StatusTooManyRequests, infra.ErrorKindRateLimited,
				"Token budget exhausted ("+reason+")")
			return true
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
		rl.dispatches++
		if err := rl.r.Context().Err(); err != nil {
			if action.cancel != nil {
				action.cancel()
			}
			action.releaseSlot()
			// prepareAndSend releases any probe it consumed on its own exit
			// paths; this break only stops the sweep.
			rl.ctxLogger.Debug("request context canceled during prepareAndSend, stopping failover sweep", "err", err)
			break
		}
		outcome := rl.handleActionResult(i, target, action)
		if outcome.handled {
			return true
		}
		// Same-target retry: a retryable failure with no later dispatchable
		// target (or one the sticky_provider policy would refuse) has no
		// failover slot, so re-attempt it. Retrying the same target is not
		// failover, so the sticky gate does not apply to it.
		if rl.consumeSameTargetRetry(i, outcome.status, outcome.retryable) {
			rl.retrySameTarget(target)
			i-- // re-attempt this target
			continue
		}
		rl.waited = false
		if reason := rl.lastFailReason; !rl.stickyAllowsFailover(reason, target) {
			rl.ctxLogger.Info("sticky_provider blocks failover, stopping sweep",
				"policy", rl.opts.Agent.StickyProvider,
				"reason", reason, "model", target.Model, "provider", target.Provider)
			break
		}
		rl.lastFailReason = failReasonNone
	}
	return false
}

// retrySameTarget logs and prepares the same-target repeat, applying the
// backoff unless handleActionError already slept for this failure.
func (rl *retryLoop) retrySameTarget(target routing.UpstreamTarget) {
	rl.ctxLogger.Info("retrying same target",
		"model", target.Model, "provider", target.Provider,
		"retry", rl.sameTargetRetries, "max", rl.maxSameTargetRetries())
	// handleActionError already slept for an upstream error action; only local
	// (network/stream) outcomes still need the backoff, escalated by the
	// local-retry index since `attempt` tracks error actions only.
	if !rl.waited {
		waitWithCancel(rl.r.Context(), calculateBackoff(rl.sameTargetRetries-1))
	}
	rl.waited = false
	rl.lastFailReason = failReasonNone
}

// consumeSameTargetRetry reports whether a retryable failure at index i should
// be re-attempted on the same target, consuming one same-target retry slot.
// It only qualifies at the final index of the sweep: while any later target
// remains (dispatchable, or merely still to be attempted and possibly skipped),
// the forward sweep proceeds and any same-target repeat belongs to that later
// attempt. This keeps the retry budget from being spent early — e.g. a 429 that
// benches the current pair while duplicate same-pair entries remain later.
// A 413 is excluded: re-sending the same oversized payload can only fail again,
// so it is failover-only.
// Named "consume" because a true result spends a slot, not a pure predicate.
func (rl *retryLoop) consumeSameTargetRetry(i int, status int, retryable bool) bool {
	if !retryable || status == http.StatusRequestEntityTooLarge ||
		i != len(rl.opts.Targets)-1 || rl.sameTargetRetries >= rl.maxSameTargetRetries() {
		return false
	}
	rl.sameTargetRetries++
	return true
}

// maxSameTargetRetries returns the same-target retry budget for this request.
// The agent's max_retries wins when positive; otherwise the governance default
// (governance.max_retry_attempts, default 3) applies so a persistently
// retryable failure on a single-target chain cannot loop forever.
func (rl *retryLoop) maxSameTargetRetries() int {
	if rl.opts.MaxRetries > 0 {
		return rl.opts.MaxRetries
	}
	return rl.gw.Config.Governance.EffectiveMaxRetryAttempts()
}

// prepareAndSend wraps the proxy's prepareAndSend method.
func (rl *retryLoop) prepareAndSend(idx int, target routing.UpstreamTarget, payload map[string]interface{}) upstreamAction {
	return rl.p.prepareAndSend(rl.gw, rl.r, idx, rl.opts.Targets, target, payload, rl.opts.Cooldown, rl.opts.TokenCount, rl.opts.AgentName, rl.opts.ApiKey, rl.stream)
}

// handleUpstreamError wraps the proxy's handleUpstreamError method.
func (rl *retryLoop) handleUpstreamError(idx int, target routing.UpstreamTarget, action upstreamAction) (retrySignal, time.Duration) {
	signal, delay := rl.p.handleUpstreamError(rl.gw, idx, rl.opts.Targets, target, rl.opts.Cooldown, rl.opts.AgentName, action)
	if rl.p.lastQuotaExhausted.Load() {
		rl.quotaExhausted = true
		rl.p.lastQuotaExhausted.Store(false)
	}
	return signal, delay
}

// Exhausted is called when all upstream targets have been exhausted without success.
// Writes an error response to the client. If quota exhaustion was detected during retries,
// returns error_kind=quota_exhausted; otherwise returns error_kind=provider_error.
func (rl *retryLoop) Exhausted() {
	// Connection-scoped (NENYA-42): a canceled client is gone — no metric,
	// no error-level log, no write attempt.
	if rl.r != nil && rl.r.Context().Err() != nil {
		model := ""
		if len(rl.opts.Targets) > 0 {
			model = rl.opts.Targets[0].Model
		}
		rl.ctxLogger.Info("client gone before exhaustion reporting", "model", model)
		return
	}
	// A retryable 4xx that consumed its retry budget is more useful to the
	// client relayed verbatim than as a generic 503: the status is the
	// upstream's own client-class verdict.
	if snap := rl.lastRetryable4xx; snap != nil {
		if rl.opts.AgentName != "" {
			rl.gw.Metrics.RecordExhausted(rl.opts.AgentName)
		}
		gwErr := ParseProviderError(snap.provider, snap.status, snap.body, nil)
		rl.ctxLogger.Warn("retry budget exhausted, relaying last upstream client error",
			"provider", snap.provider, "status", snap.status)
		rl.writeUpstreamErrorToClient(snap.status, gwErr)
		return
	}
	rl.ctxLogger.Error("all upstream targets exhausted",
		"total", len(rl.opts.Targets), "attempts", rl.attempt, "dispatches", rl.dispatches, "token_count", rl.opts.TokenCount)
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
	if rl.lastStreamErr != nil {
		// Last target died from a transport-level stream read error rather than
		// an empty stream. Log the cause for operators; the client message
		// stays generic so raw error text does not leak network topology.
		rl.ctxLogger.Error("last upstream target failed with stream read error",
			"err", rl.lastStreamErr)
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

func handleUpstreamResponse(ctxLogger *slog.Logger, resp *http.Response, cancel context.CancelFunc, release func()) upstreamAction {
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(strings.ToLower(ct), "text/html") {
		ctxLogger.Warn("upstream returned HTML instead of API response, skipping target",
			"content_type", ct, "status", resp.StatusCode)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		cancel()
		return upstreamAction{kind: actionContinue, release: release}
	}
	isSSE := strings.Contains(strings.ToLower(ct), "text/event-stream")
	if isSSE {
		return upstreamAction{kind: actionStream, resp: resp, cancel: cancel, release: release}
	}
	return upstreamAction{kind: actionResponse, resp: resp, cancel: cancel, release: release}
}

// upstreamRequestContext builds the context for an outbound upstream call.
// Streaming requests use an unbounded cancelable context — the stream idle
// stall reader enforces inactivity limits — while non-streaming calls honor
// the provider's TimeoutSeconds when set, falling back to the governance
// upstream timeout and finally an unbounded context.
func upstreamRequestContext(base context.Context, gw *gateway.NenyaGateway, target routing.UpstreamTarget, streaming bool) (context.Context, context.CancelFunc) {
	if streaming {
		return context.WithCancel(base)
	}
	if pr, ok := gw.Providers[target.Provider]; ok && pr.TimeoutSeconds > 0 {
		return context.WithTimeout(base, time.Duration(pr.TimeoutSeconds)*time.Second)
	}
	timeout := gw.Config.Governance.EffectiveUpstreamTimeout()
	if timeout > 0 {
		return context.WithTimeout(base, timeout)
	}
	return context.WithCancel(base)
}

func resolveCacheSalt(apiKey *config.ApiKey, agentName string, cfg *config.Config) string {
	if apiKey != nil && apiKey.CacheSalt != nil && *apiKey.CacheSalt != "" {
		return *apiKey.CacheSalt
	}
	if agent, ok := cfg.Agents[agentName]; ok && agent.CacheSalt != nil && *agent.CacheSalt != "" {
		return *agent.CacheSalt
	}
	return ""
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
	apiKey *config.ApiKey,
	streaming bool,
) upstreamAction {
	ctxLogger := gw.Logger.With(
		"operation", "upstream",
		"agent", agentName,
		"provider", target.Provider,
		"model", target.Model,
		"target_idx", fmt.Sprintf("%d/%d", idx+1, len(targets)),
	)

	gw.Stats.RecordRequest(target.Model, tokenCount)
	gw.Metrics.RecordUpstreamRequest(target.Model, agentName, target.Provider)

	if action := p.checkPreDispatchGuards(gw, ctxLogger, target, tokenCount, agentName); action != nil {
		return *action
	}

	// Per-model concurrency admission control (NENYA-69): hold a slot for
	// the entire upstream round-trip, including SSE stream consumption.
	// Queue-and-wait: the request blocks here (ctx-aware) until a slot
	// frees instead of colliding with the upstream concurrency cap.
	concLimit := gw.EffectiveConcurrencyLimit(target.Provider, target.Model)
	waitStart := time.Now()
	release, acqErr := gw.ConcurrencyLimiter.Acquire(r.Context(), target.Provider+"/"+target.Model, concLimit)
	if acqErr != nil {
		// Client gone while queued: no circuit-breaker pollution, no write.
		// Release the half-open probe slot consumed by the pre-dispatch guard
		// so an abandoned probe cannot wedge the circuit.
		gw.Metrics.RecordConcurrencyRejected(target.Provider, target.Model)
		ctxLogger.Info("client canceled while waiting for concurrency slot",
			"model", target.Model, "provider", target.Provider)
		gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
		return upstreamAction{kind: actionContinue}
	}
	gw.Metrics.RecordConcurrencyWait(target.Provider, target.Model, time.Since(waitStart))
	gw.Metrics.IncConcurrencyInflight(target.Provider, target.Model)
	release = wrapConcurrencyRelease(release, gw, target.Provider, target.Model)

	transformDeps := routing.TransformDeps{
		Logger:             gw.Logger,
		Providers:          gw.Providers,
		Config:             &gw.Config,
		ThoughtSigCache:    gw.ThoughtSigCache,
		ExtractContentText: gateway.ExtractContentText,
		Catalog:            gw.ModelCatalog,
		CountTokens:        gw.CountTokens,
		AgentName:          agentName,
		CacheSalt:          resolveCacheSalt(apiKey, agentName, &gw.Config),
		Metrics:            gw.Metrics,
	}
	transformedBody, _, err := routing.TransformRequestForUpstream(transformDeps, target.Provider, target.URL, payload, target.Model, target.MaxOutput, target.MaxContext, target.Format, target.ReasoningEffort)
	if err != nil {
		ctxLogger.Warn("failed to transform request, using original payload", "err", err)
		transformedBody, _ = json.Marshal(payload)
	}

	// Record the input estimate AFTER transform so the value reflects the
	// payload actually dispatched (post trim/window/bouncer), making
	// direction="client" − direction="input" the whole-pipeline savings.
	gw.Metrics.RecordTokens("input", target.Model, agentName, target.Provider, gw.CountTokens(string(transformedBody)))

	req, err := p.buildUpstreamRequest(gw, r.Context(), r.Method, target.URL, transformedBody, target.Provider, target.Model, target.Credential, r.Header)
	if err != nil {
		ctxLogger.Error("failed to create upstream request", "err", err)
		// No dispatch happened: release the probe slot consumed by the guard
		// so an abandoned half-open probe cannot wedge the circuit.
		gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
		return upstreamAction{kind: actionContinue, release: release}
	}

	logRequestIfDebug(r.Context(), ctxLogger, req, target.URL, transformedBody)

	// Streaming keeps an unbounded context (bounded by the stream idle stall
	// reader); non-streaming calls are bounded by the provider timeout.
	upstreamCtx, upstreamCancel := upstreamRequestContext(r.Context(), gw, target, streaming)
	req = req.WithContext(upstreamCtx)

	startTime := time.Now()
	resp, err := gw.ClientFor(target.Provider).Do(req)
	if err != nil {
		upstreamCancel()
		p.recordNetworkError(ctxLogger, gw, target, err, r, cooldownDuration)
		// Network failures are transient: retryable for a same-target repeat
		// when no later target remains (bounded by the attempt cap).
		return upstreamAction{kind: actionContinue, release: release, retryable: true}
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
		return upstreamAction{kind: actionError, resp: resp, cancel: upstreamCancel, release: release}
	}

	return handleUpstreamResponse(ctxLogger, resp, upstreamCancel, release)
}

// wrapConcurrencyRelease ties the in-flight gauge to the slot release so the
// occupancy metric mirrors exactly when slots are held.
func wrapConcurrencyRelease(release func(), gw *gateway.NenyaGateway, provider, model string) func() {
	return sync.OnceFunc(func() {
		release()
		gw.Metrics.DecConcurrencyInflight(provider, model)
	})
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
	if allowed, rej := gw.RateLimiter.CheckDetailed(target.Provider, target.URL, tokenCount); !allowed {
		gw.Metrics.RecordRateLimitRejected(infra.ExtractHost(target.URL))
		ctxLogger.Warn("target skipped: rate limit exceeded",
			"dimension", rej.Dimension,
			"limit", rej.Limit,
			"bucket_left", rej.BucketLeft,
			"token_count", rej.TokenCount)
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

// extendForCooldownSignal returns the longer of the current effective
// cooldown and a detected signal, logging when the signal wins.
func extendForCooldownSignal(logger *slog.Logger, effective, signal time.Duration, reason string) time.Duration {
	if signal <= effective {
		return effective
	}
	logger.Info(reason, "cooldown_s", signal.Seconds())
	return signal
}

// mergeBodyQuotaSignals folds body-side quota signals into the effective
// cooldown. Detection (isQuota) is decoupled from extension: a quota signal
// below the configured cooldown still flags quota exhaustion for the
// error-kind contract and the NENYA-41 floor.
func mergeBodyQuotaSignals(logger *slog.Logger, errorBody []byte, effective time.Duration) (time.Duration, bool) {
	if len(errorBody) == 0 {
		return effective, false
	}
	isQuota := false
	if quotaCD := parseQuotaExhaustion(errorBody, logger); quotaCD > 0 {
		isQuota = true
		effective = extendForCooldownSignal(logger, effective, quotaCD, "quota exhaustion detected, extending cooldown")
	}
	if resetCD := parseQuotaResetFields(errorBody); resetCD > 0 {
		isQuota = true
		effective = extendForCooldownSignal(logger, effective, resetCD, "quota reset field detected, extending cooldown")
	}
	return effective, isQuota
}

// derive429Cooldown merges the configured cooldown with upstream-declared
// windows (NENYA-43), longest wins: body quota patterns, flexible body reset
// fields, and header windows (Retry-After / Anthropic unified resets /
// x-ratelimit-reset). Parsing is 429-gated to preserve existing semantics —
// 5xx errors keep serverCooldown regardless of body wording. Returns the
// cooldown and whether a quota signal was seen.
func derive429Cooldown(logger *slog.Logger, errorBody []byte, header http.Header, status int, cooldownDuration time.Duration) (time.Duration, bool) {
	if status != http.StatusTooManyRequests {
		return cooldownDuration, false
	}
	effective, isQuota := mergeBodyQuotaSignals(logger, errorBody, cooldownDuration)
	if upstreamCD := deriveUpstreamRateLimitCooldown(header); upstreamCD > effective {
		effective = upstreamCD
		logger.Info("upstream rate-limit window cooldown derived", "cooldown_s", effective.Seconds())
	}
	return effective, isQuota
}

func handleRetryableError429(logger *slog.Logger, errorBody []byte, action upstreamAction, cooldownDuration time.Duration, target routing.UpstreamTarget, agentName string, gw *gateway.NenyaGateway) (time.Duration, bool) {
	effectiveCooldown, isQuota := derive429Cooldown(logger, errorBody, action.resp.Header, action.resp.StatusCode, cooldownDuration)

	if action.resp.StatusCode == http.StatusTooManyRequests {
		// Quota floor (NENYA-41): never let a sub-second provider-supplied
		// cooldown produce a zero-wait retry storm against the same account.
		if floor := gw.Config.Governance.EffectiveMinQuotaCooldown(); isQuota && floor > 0 && effectiveCooldown < floor {
			effectiveCooldown = floor
			logger.Info("quota cooldown floored", "cooldown_s", effectiveCooldown.Seconds())
		}
		// Idle-connection eviction (NENYA-47): an exhausted account's pooled
		// connections are dead weight — drop them while the account cools
		// down. Called without gateway locks held.
		gw.EvictIdleConnections(target.Provider)
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

// handleUpstreamError classifies an upstream error action and returns the
// loop signal plus any upstream-declared retry delay. retrySignalRetry marks
// a transient/retryable failure (same-target eligible when no later target
// remains); retrySignalContinue marks a permanent client error with a later
// target to fail over to; retrySignalDone means the caller must surface the
// error to the client.
func (p *Proxy) handleUpstreamError(gw *gateway.NenyaGateway,
	idx int,
	targets []routing.UpstreamTarget,
	target routing.UpstreamTarget,
	cooldownDuration time.Duration,
	agentName string,
	action upstreamAction,
) (retrySignal, time.Duration) {
	errorBody := action.body

	ctxLogger := gw.Logger.With(
		"operation", "upstream_error",
		"agent", agentName,
		"provider", target.Provider,
		"model", target.Model,
		"target_idx", fmt.Sprintf("%d/%d", idx+1, len(targets)),
		"status", action.resp.StatusCode,
	)

	errClass := adapter.ForProvider(target.Provider).NormalizeError(action.resp.StatusCode, action.body)
	// Concurrency-limit rejection (NENYA-69): e.g. ZAI 1302. Hitting a
	// per-model in-flight cap is saturation, not provider illness — do not
	// activate a cooldown or count a circuit-breaker failure. A short fixed
	// wait lets a just-freed slot settle before the sweep proceeds.
	if errClass == adapter.ErrorConcurrencyLimited {
		gw.Metrics.RecordConcurrencyLimited(target.Provider, target.Model)
		ctxLogger.Warn("upstream concurrency limit hit, retrying without cooldown",
			"model", target.Model, "provider", target.Provider)
		// Saturation is not a circuit outcome: release the half-open probe
		// slot consumed by the pre-dispatch guard so repeated retries cannot
		// exhaust the probe budget and wedge the circuit.
		gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
		return retrySignalRetry, concurrencyRetryDelay()
	}

	if p.isRetryableStatus(gw, target.Provider, action.resp.StatusCode) {
		logRetryableError(ctxLogger, errorBody, gw)
		delay, isQuota := handleRetryableError429(ctxLogger, errorBody, action, cooldownDuration, target, agentName, gw)
		if isQuota {
			p.lastQuotaExhausted.Store(true)
		}
		return retrySignalRetry, delay
	}

	// Retryable 4xx: built-in body patterns, provider-configured phrases, and
	// — when governance.retry_opaque_4xx is enabled — opaque JSON bodies a
	// gateway relayed without an error envelope. Classified retryable
	// regardless of how many targets remain: Run advances when a later
	// target exists and re-attempts this one when it is the last.
	if p.recordRetryable4xx(gw, ctxLogger, target, action, errorBody) {
		return retrySignalRetry, 0
	}

	if retryable, isQuota := handleAdapterRetryableError(ctxLogger, target, action, cooldownDuration, gw); retryable {
		if isQuota {
			p.lastQuotaExhausted.Store(true)
		}
		return retrySignalRetry, 0
	}

	defer action.cancel()
	// A permanent client-class error records no circuit outcome; release the
	// half-open probe slot consumed by the pre-dispatch guard so repeated
	// client errors cannot exhaust the probe budget.
	gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
	if idx+1 < len(targets) {
		logWarnRetryable(ctxLogger, errorBody, gw, "non-retryable upstream error, trying next target")
		return retrySignalContinue, 0
	}

	logErrorRetryable(ctxLogger, errorBody, gw, "non-retryable upstream error, no more targets")
	return retrySignalDone, 0
}

// recordRetryable4xx classifies a 4xx body and, when retryable, logs the
// reason and records the appropriate circuit-breaker signal. Returns true when
// the caller should treat the error as retryable.
func (p *Proxy) recordRetryable4xx(gw *gateway.NenyaGateway, ctxLogger *slog.Logger, target routing.UpstreamTarget, action upstreamAction, errorBody []byte) bool {
	reason := retryable4xxReasonFor(gw, action.resp.StatusCode, errorBody, target.Provider)
	if reason == retryable4xxNone {
		return false
	}
	logBody := redactForLog(string(errorBody), gw)
	if reason == retryable4xxPhrase {
		ctxLogger.Warn("provider-configured retryable phrase matched", "reason", string(reason), "body", logBody)
	} else {
		ctxLogger.Warn("retryable client error from upstream", "reason", string(reason), "body", logBody)
	}
	// An opaque body is a heuristic: count the request without contributing
	// to the failure threshold, so a client provoking an ambiguous body
	// cannot bench a healthy provider. Pattern and phrase matches are
	// deliberate signals and record normally.
	if reason == retryable4xxOpaque {
		gw.AgentState.CB.RecordOpaqueFailure(target.CoolKey)
	} else {
		gw.AgentState.RecordFailureWithStatus(target, action.resp.StatusCode, string(errorBody))
	}
	return true
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
func (p *Proxy) attemptContextLimitSummarization(ctx context.Context, ctxLogger *slog.Logger, gw *gateway.NenyaGateway, originalPayload []byte, errorBody []byte, agentName, providerName, modelName string, canary pipeline.CanarResult) (map[string]interface{}, error) {
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

	// The canary marker must not reach the summarizer: a faithful
	// summary would reproduce the token verbatim and trip the wire on
	// the retried conversation. It is re-appended verbatim below.
	messages = canarySafeMessages(messages, canary)

	summarized, err := p.summarizeMessages(ctx, gw, messages, agentName, providerName, modelName)
	if err != nil {
		return nil, err
	}

	newPayload := make(map[string]interface{}, len(payload))
	for k, v := range payload {
		newPayload[k] = v
	}
	// The canary marker (a trailing system message) is part of the
	// original conversation but is not summarizable content: re-append
	// it verbatim so the tripwire stays armed on the summarized retry.
	if canary.Token != "" {
		summarized = append(summarized, map[string]interface{}{
			"role":    "system",
			"content": pipeline.CanaryMarker(canary.Token),
		})
	}
	newPayload["messages"] = summarized
	return newPayload, nil
}

// canarySafeMessages strips canary material from the summarizer input:
// the injected marker message is removed entirely and a bare token
// echoed into other history content (e.g. log-mode-allowed tool output)
// is scrubbed, so the summary can never reproduce the token.
func canarySafeMessages(messages []interface{}, canary pipeline.CanarResult) []interface{} {
	if canary.Token == "" {
		return messages
	}
	filtered := make([]interface{}, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			filtered = append(filtered, m)
			continue
		}
		if msg["content"] == pipeline.CanaryMarker(canary.Token) {
			continue
		}
		switch text := msg["content"].(type) {
		case string:
			if strings.Contains(text, canary.Token) {
				msg["content"] = strings.ReplaceAll(text, canary.Token, "")
			}
		case []interface{}:
			// Content-array form: scrub bare tokens from text parts.
			for _, part := range text {
				pm, ok := part.(map[string]interface{})
				if !ok || pm["type"] != "text" {
					continue
				}
				if pt, ok := pm["text"].(string); ok && strings.Contains(pt, canary.Token) {
					pm["text"] = strings.ReplaceAll(pt, canary.Token, "")
				}
			}
		}
		filtered = append(filtered, m)
	}
	return filtered
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

// matchRequestScopedError returns the provider-configured
// request_scoped_errors rule the upstream error matches (NENYA-42): the
// client's payload is at fault regardless of the status class. Rules with a
// nonzero Status apply only to that status; rules with an empty
// MessagePattern never match. Returns nil when no rule matches.
func (p *Proxy) matchRequestScopedError(gw *gateway.NenyaGateway, providerName string, statusCode int, body []byte) *config.RequestScopedErrorRule {
	pr, ok := gw.Providers[providerName]
	if !ok || len(pr.RequestScopedErrors) == 0 {
		return nil
	}
	lowerBody := strings.ToLower(string(body))
	for i := range pr.RequestScopedErrors {
		rule := &pr.RequestScopedErrors[i]
		if rule.Status != 0 && rule.Status != statusCode {
			continue
		}
		pattern := strings.TrimSpace(rule.MessagePattern)
		if pattern == "" {
			continue
		}
		if strings.Contains(lowerBody, strings.ToLower(pattern)) {
			return rule
		}
	}
	return nil
}

// writeUpstreamErrorToClient relays a parsed upstream error to the client in
// the wire format the request arrived in (SSE error frame for streams, JSON
// envelope otherwise).
func (rl *retryLoop) writeUpstreamErrorToClient(statusCode int, gwErr *GatewayError) {
	if rl.stream {
		writeGatewayStreamError(rl.w, statusCode, gwErr.Type, gwErr.Message)
		return
	}
	writeGatewayError(rl.w, statusCode, gwErr.Type, gwErr.Message)
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

// upstreamBlamePatterns are provider-agnostic error-body phrases indicating
// that a 4xx response actually carries a relayed upstream/transient failure
// (aggregators and gateways often wear their upstream's error as a client
// error). Matched case-insensitively in addition to the per-provider sets.
// Known trade-off: providers that echo prompt snippets in error bodies can
// false-positive on the generic phrases; bounded by the multi-target sweep
// and MaxRetries.
var upstreamBlamePatterns = []string{
	"upstream request failed",
	"upstream error",
	"upstream unavailable",
	"upstream is unavailable",
	"reach upstream",
}

var xaiRetryablePatterns = []string{
	"at capacity",
	"temporarily unavailable",
	"service overloaded",
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
	// name matching is the existing substring heuristic shared by all
	// matchers; "xai" may over-match operator-chosen names (worst case: one
	// wasted retry of a permanent 400).
	{name: "xai", patterns: xaiRetryablePatterns},
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

	for _, pat := range upstreamBlamePatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}

	for _, pat := range commonRetryablePatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}

	return matchProviderSpecificPatterns(lower, provider)
}

// errorEnvelopeKeys are the JSON object keys that identify a provider error
// envelope. A 4xx body carrying any of them is a deliberate, structured
// client error; a JSON object with none of them is treated as an opaque
// relayed failure (see opaque4xxBody).
var errorEnvelopeKeys = []string{
	"error", "errors", "detail", "details", "message", "type", "code", "title", "reason", "status",
}

// opaque4xxBody reports whether a 400/422 response body is a non-empty JSON
// object carrying none of the recognized error-envelope keys — the shape
// aggregators and gateways produce when they relay an upstream failure without
// wrapping it (e.g. `{"model":"..."}`). Empty, empty-object, non-JSON,
// non-object, and enveloped bodies all return false, so genuine client errors
// stay non-retryable.
//
// 413 (Request Entity Too Large) is deliberately excluded: the payload is an
// immutable part of an identical retry, so both a same-target repeat and a
// failover would re-send the same oversized body (possibly leaking it to
// another provider) and fail again.
func opaque4xxBody(statusCode int, body []byte) bool {
	switch statusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
	default:
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || len(obj) == 0 {
		return false
	}
	for _, key := range errorEnvelopeKeys {
		if _, ok := obj[key]; ok {
			return false
		}
	}
	return true
}

// retryable4xxReason classifies why a 4xx body is retryable (empty = not).
type retryable4xxReason string

const (
	retryable4xxNone    retryable4xxReason = ""
	retryable4xxPattern retryable4xxReason = "pattern"
	retryable4xxPhrase  retryable4xxReason = "phrase"
	retryable4xxOpaque  retryable4xxReason = "opaque"
)

// retryable4xxReason reports the classifier verdict for a 4xx body, along with
// whether the failure is safe to re-send unchanged to the *same* target. A 413
// whose body matches a context/max_tokens pattern is retryable (another target
// may accept the payload), but repeating it in place can only fail again, so
// callers must fail over rather than same-target retry.
func retryable4xxReasonFor(gw *gateway.NenyaGateway, statusCode int, body []byte, provider string) retryable4xxReason {
	if isRetryableClientErrorForProvider(statusCode, body, provider) {
		return retryable4xxPattern
	}
	if provider != "" && gw != nil {
		if pr, ok := gw.Providers[provider]; ok && pr != nil && statusCode >= 400 && statusCode < 500 &&
			len(pr.RetryablePhrases) > 0 && bodyContainsAnyPhrases(body, pr.RetryablePhrases) {
			return retryable4xxPhrase
		}
	}
	if gw != nil && gw.Config.Governance.Opaque4xxRetryEnabled() && opaque4xxBody(statusCode, body) {
		return retryable4xxOpaque
	}
	return retryable4xxNone
}

// bodyContainsAnyPhrases reports whether body contains any of the given
// phrases, case-insensitively. Used for provider-configured
// retryable_phrases.
func bodyContainsAnyPhrases(body []byte, phrases []string) bool {
	if len(body) == 0 || len(phrases) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, phrase := range phrases {
		if strings.TrimSpace(phrase) == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(phrase)) {
			return true
		}
	}
	return false
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
