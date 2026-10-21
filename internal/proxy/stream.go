package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
	providerpkg "github.com/nenya/internal/providers"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
)

// streamBufferSize is the size of buffers used in the streaming hot path (32KB).
// This is a balanced tradeoff between allocation overhead and per-read throughput.
const (
	streamBufferSize = 32 * 1024
	// streamHeadBufferSize bounds the probe read for the pre-header stream-head
	// check; enough to classify the opening SSE events of typical upstreams.
	streamHeadBufferSize = 4096
	// TokenDirectionReasoning is the direction label for reasoning token metrics
	TokenDirectionReasoning = "reasoning"
)

// streamingBufPool is a sync.Pool for 32KB read/transfer buffers used in the
// streaming hot path. Buffers are resliced during use and restored to full
// length when returned via putStreamBuffer.
var streamingBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, streamBufferSize)
		return &b
	},
}

// probeStreamHead reads the first chunk of the upstream stream before any
// headers are committed to the client and returns a non-nil streamResult when
// the upstream must be treated as a failure:
//   - empty body (governance.empty_stream_as_error): fall back to the next target;
//   - the first SSE event is an upstream error with no content before it
//     (governance.early_stream_error_failover): fall back to the next target
//     when one exists; on the last target the error is forwarded unchanged.
//
// When the stream is healthy (or the early error is not actionable), the first
// buffered chunk is preserved via a prefixedReadCloser so no bytes are lost.
//
// opts is passed whole rather than extracting idx/targets so the receiver stays
// within the project's <=5 parameter guideline (see AGENTS.md §11).
//
// stallR, when non-nil, is the stall-bounded reader built around the upstream
// body; the probe reads through it so stream_idle_timeout_seconds bounds the
// head read as well, and the stream phase must keep using the same reader so
// read-ahead bytes buffered by its goroutine are not lost.
func (p *Proxy) probeStreamHead(gw *gateway.NenyaGateway, action upstreamAction, target routing.UpstreamTarget, cooldownDuration time.Duration, opts streamResponseOpts, stallR *stallReader) *streamResult {
	// Bootstrap buffering (NENYA-45) subsumes the first-event probe when
	// enabled: it holds handshake events until real output vs rejection is
	// decidable, keeping failover possible against in-stream rejections that
	// arrive after metadata frames.
	if budget := resolveBootstrapBuffer(gw, target.Provider); budget > 0 {
		return p.bootstrapStreamHead(bootstrapProbeParams{
			gw:            gw,
			action:        action,
			target:        target,
			cooldown:      cooldownDuration,
			opts:          opts,
			stallR:        stallR,
			budget:        budget,
			emptyFailover: gw.Config.Governance.EmptyStreamAsError != nil && *gw.Config.Governance.EmptyStreamAsError,
			maxHold:       time.Duration(gw.Config.Governance.StreamBootstrapMaxHoldSeconds) * time.Second,
		})
	}
	probe := headProbeEnabled(gw.Config.Governance)
	if !probe.enabled {
		return nil
	}
	// Single use, once per stream request: a pooled buffer would only shift a
	// one-time allocation off the request's stack frame without measurable gain.
	firstBuf := make([]byte, streamHeadBufferSize)
	bodyReader := action.resp.Body
	if stallR != nil {
		bodyReader = stallR
	}
	n, readErr := bodyReader.Read(firstBuf)
	if n == 0 {
		if res := p.handleStreamHeadNoData(gw, action, target, cooldownDuration, probe.empty, readErr, stallR); res != nil {
			return res
		}
		return nil
	}

	chunk := firstBuf[:n]
	kind := streamHeadKind(headUndetermined)
	if probe.earlyError {
		kind = classifyStreamHead(chunk)
	}
	if kind == headError && opts.idx+1 < len(opts.targets) {
		return p.failoverEarlyHeadError(gw, action, target, cooldownDuration, stallR, "failover")
	}

	// Wrap so the probe's first chunk is served before the stall reader
	// continues. Any read-ahead bytes the reader already buffered (remainBuf
	// or a queued channel result) stay in line behind the prefix — nothing is
	// lost, because the stream phase keeps reading through this same reader.
	action.resp.Body = &prefixedReadCloser{
		prefix: chunk,
		reader: bodyReader,
	}
	if kind == headError {
		// Last (or only) target: the error cannot be failed over, so it is
		// forwarded to the client as today, but its circuit-breaker impact is
		// recorded so the provider is de-prioritized on later requests.
		gw.Metrics.RecordEarlyStreamError(target.Model, target.Provider, "forwarded_last_target")
		gw.Logger.Warn("upstream error event at stream head, no alternate target; forwarding to client",
			"model", target.Model, "provider", target.Provider)
	}
	return nil
}

// handleStreamHeadNoData resolves a stream-head probe read that returned zero
// bytes. A clean EOF (or nil error) with empty-stream detection enabled fails
// over via streamResult{empty}; a transport-level read error is returned as-is
// via streamResult{err} so the retry loop can distinguish it from emptiness.
// Returns nil when the stream should continue unchanged.
func (p *Proxy) handleStreamHeadNoData(gw *gateway.NenyaGateway, action upstreamAction, target routing.UpstreamTarget, cooldownDuration time.Duration, probeEmpty bool, readErr error, stallR *stallReader) *streamResult {
	if readErr == nil || readErr == io.EOF {
		if !probeEmpty {
			// NENYA-160: with the empty-failover flag off, the stream still
			// flows to the SSE reader, whose empty-stream handler carries the
			// always-on signal — counting here too would double-count.
			return nil
		}
		if err := action.resp.Body.Close(); err != nil {
			gw.Logger.Debug("close empty upstream body", "err", err, "provider", target.Provider)
		}
		if stallR != nil {
			stallR.Stop()
			stallR.DrainBuffered()
		}
		gw.AgentState.RecordFailure(target, cooldownDuration)
		gw.Metrics.RecordEmptyStream(target.Model, target.Provider)
		// NENYA-160: on this path the body is closed before any SSE reader
		// exists, so this is the empty stream's only always-on count.
		gw.Metrics.RecordEmptyResponse(target.Model, target.Provider, "stream")
		gw.Logger.Warn("empty upstream stream detected, falling back to next target",
			"model", target.Model, "provider", target.Provider)
		return &streamResult{empty: true}
	}
	// Transport-level failure before any bytes: distinct from a genuinely
	// empty stream. Fail over without mislabeling it as empty; the retry
	// loop surfaces the error on the last target.
	if err := action.resp.Body.Close(); err != nil {
		gw.Logger.Debug("close errored upstream body", "err", err, "provider", target.Provider)
	}
	if stallR != nil {
		stallR.Stop()
		stallR.DrainBuffered()
	}
	action.cancel()
	gw.AgentState.RecordFailure(target, cooldownDuration)
	gw.Logger.Warn("upstream stream read error at head, falling back to next target",
		"err", readErr, "model", target.Model, "provider", target.Provider)
	return &streamResult{err: readErr}
}

// bootstrapProbeParams groups the bootstrap buffering probe inputs (AGENTS.md
// §11 parameter grouping).
type bootstrapProbeParams struct {
	gw     *gateway.NenyaGateway
	action upstreamAction
	target routing.UpstreamTarget
	// cooldown is the cooldown applied to the failed target on rejection.
	cooldown time.Duration
	// opts carries the retry-loop position (idx/targets) for failover checks.
	opts streamResponseOpts
	// stallR, when non-nil, is the stall-bounded reader the stream phase
	// reuses; bootstrap reads go through it so stream_idle_timeout_seconds
	// bounds the handshake hold as well.
	stallR *stallReader
	// budget is the maximum bytes held before degrading to unbuffered.
	budget int
	// maxHold is the optional time ceiling for the buffering phase
	// (NENYA-174): 0 = unbounded (the byte budget is the only bound).
	maxHold time.Duration
	// emptyFailover mirrors governance.empty_stream_as_error for the
	// zero-bytes-read case.
	emptyFailover bool
}

// bootstrapHoldExceeded is the production hold-time predicate (NENYA-174):
// a non-positive ceiling never trips; the ceiling trips once elapsed. Used
// by the bootstrap loop and pinned directly by its fixture.
func bootstrapHoldExceeded(elapsed, maxHold time.Duration) bool {
	if maxHold <= 0 {
		return false
	}
	return elapsed >= maxHold
}

// bootstrapStreamHead holds the upstream stream's handshake events until the
// stream is decidable (NENYA-45): an allow-listed output event flushes the
// buffer and commits the stream unchanged; an in-stream rejection fails over
// to the next target synchronously, before the 200 is committed; a budget
// overflow degrades to unbuffered streaming. No client bytes are written
// until a decision is reached, so a rejection stays failover-able.
func (p *Proxy) bootstrapStreamHead(pp bootstrapProbeParams) *streamResult {
	bodyReader := pp.action.resp.Body
	if pp.stallR != nil {
		bodyReader = pp.stallR
	}
	acc := make([]byte, 0, streamHeadBufferSize)
	scratch := make([]byte, streamHeadBufferSize)
	offset := 0
	start := time.Now()
	for {
		if bootstrapHoldExceeded(time.Since(start), pp.maxHold) {
			// Time ceiling (NENYA-174): checked every iteration — including
			// after a read that returned nothing — so only a provider that
			// stalls silently (bounded separately by the stall reader) can
			// hold the 200 past the ceiling.
			pp.gw.Metrics.RecordStreamBootstrap(pp.target.Model, pp.target.Provider, "hold-timeout")
			pp.gw.Metrics.RecordBootstrapHold(pp.target.Model, pp.target.Provider, time.Since(start))
			pp.gw.Logger.Debug("stream bootstrap hold-time ceiling, degrading to unbuffered",
				"model", pp.target.Model, "provider", pp.target.Provider, "bytes", len(acc))
			return p.flushBootstrapPrefix(pp, acc)
		}
		n, readErr := bodyReader.Read(scratch)
		if n > 0 {
			acc = append(acc, scratch[:n]...)
			verdict, newOffset := scanBootstrapBuffer(acc, offset)
			offset = newOffset
			switch verdict {
			case bootstrapReject:
				return p.resolveBootstrapRejection(pp, acc, offset, start)
			case bootstrapRefusal:
				return p.resolveBootstrapRefusal(pp, acc, start)
			case bootstrapOutput:
				pp.gw.Metrics.RecordStreamBootstrap(pp.target.Model, pp.target.Provider, "flushed")
				pp.gw.Metrics.RecordBootstrapHold(pp.target.Model, pp.target.Provider, time.Since(start))
				return p.flushBootstrapPrefix(pp, acc)
			}
			if len(acc) >= pp.budget {
				// Handshake grew past the budget without deciding: degrade to
				// unbuffered streaming. Post-commit errors are typed stream
				// errors (no retry) — first-byte commit is the trade-off.
				pp.gw.Metrics.RecordStreamBootstrap(pp.target.Model, pp.target.Provider, "overflow")
				pp.gw.Metrics.RecordBootstrapHold(pp.target.Model, pp.target.Provider, time.Since(start))
				pp.gw.Logger.Debug("stream bootstrap buffer overflow, degrading to unbuffered",
					"model", pp.target.Model, "provider", pp.target.Provider, "bytes", len(acc))
				return p.flushBootstrapPrefix(pp, acc)
			}
			if bootstrapHoldExceeded(time.Since(start), pp.maxHold) {
				// Time ceiling (NENYA-174): a slow-drip provider cannot hold
				// the 200 hostage past the configured hold time — degrade to
				// unbuffered streaming exactly like a byte overflow.
				pp.gw.Metrics.RecordStreamBootstrap(pp.target.Model, pp.target.Provider, "hold-timeout")
				pp.gw.Metrics.RecordBootstrapHold(pp.target.Model, pp.target.Provider, time.Since(start))
				pp.gw.Logger.Debug("stream bootstrap hold-time ceiling, degrading to unbuffered",
					"model", pp.target.Model, "provider", pp.target.Provider, "bytes", len(acc))
				return p.flushBootstrapPrefix(pp, acc)
			}
			if readErr != nil {
				// Delivered bytes alongside an error: flush what was scanned;
				// the copy phase surfaces the read error downstream.
				return p.flushBootstrapPrefix(pp, acc)
			}
			continue
		}
		if len(acc) > 0 {
			// Stream ended during the handshake without a decision: deliver
			// the scanned prefix (the SSE cut machinery handles the ending).
			return p.flushBootstrapPrefix(pp, acc)
		}
		if res := p.handleStreamHeadNoData(pp.gw, pp.action, pp.target, pp.cooldown, pp.emptyFailover, readErr, pp.stallR); res != nil {
			return res
		}
		return nil
	}
}

// resolveBootstrapRejection handles an in-stream rejection detected before
// the 200 is committed: with an alternate target it fails over synchronously
// (classifying/circuit-breaking like any upstream error); on the last target
// the buffered error stream is forwarded as today. scanEnd bounds the
// policy-wording scan to the classified prefix (bytes up to the decisive
// line), so a policy frame after a genuine provider-fault frame cannot
// downgrade the charge — while the forward still flushes the FULL buffer. A
// policy-worded rejection (safeguard / content-policy, NENYA-191) fails over
// WITHOUT the charge: the model declining the content is not a provider
// fault; on the last target it is forwarded, equally uncharged.
func (p *Proxy) resolveBootstrapRejection(pp bootstrapProbeParams, acc []byte, scanEnd int, start time.Time) *streamResult {
	pp.gw.Metrics.RecordBootstrapHold(pp.target.Model, pp.target.Provider, time.Since(start))
	if bufferedHasPolicyRefusal(acc[:scanEnd]) {
		if pp.opts.idx+1 < len(pp.opts.targets) {
			pp.gw.Metrics.RecordStreamBootstrap(pp.target.Model, pp.target.Provider, "refusal_failover")
			pp.gw.Metrics.RecordEarlyStreamError(pp.target.Model, pp.target.Provider, "refusal_failover")
			pp.gw.Logger.Warn("policy rejection before commit, failing over without breaker charge",
				"model", pp.target.Model, "provider", pp.target.Provider, "held_bytes", len(acc))
			return p.failoverHeadPolicy(pp, streamFailoverPolicyRefusal)
		}
		pp.gw.Metrics.RecordStreamBootstrap(pp.target.Model, pp.target.Provider, "refusal_forwarded")
		pp.gw.Metrics.RecordEarlyStreamError(pp.target.Model, pp.target.Provider, "refusal_forwarded")
		pp.gw.Logger.Warn("policy rejection before commit, no alternate target; forwarding without breaker charge",
			"model", pp.target.Model, "provider", pp.target.Provider, "held_bytes", len(acc))
		return p.flushBootstrapPrefix(pp, acc)
	}
	if pp.opts.idx+1 < len(pp.opts.targets) {
		pp.gw.Metrics.RecordStreamBootstrap(pp.target.Model, pp.target.Provider, "rejected_failover")
		pp.gw.Logger.Warn("in-stream rejection before commit, failing over",
			"model", pp.target.Model, "provider", pp.target.Provider, "held_bytes", len(acc))
		return p.failoverEarlyHeadError(pp.gw, pp.action, pp.target, pp.cooldown, pp.stallR, "bootstrap_failover")
	}
	pp.gw.Metrics.RecordStreamBootstrap(pp.target.Model, pp.target.Provider, "rejected_forwarded")
	pp.gw.Metrics.RecordEarlyStreamError(pp.target.Model, pp.target.Provider, "bootstrap_forwarded")
	pp.gw.Logger.Warn("in-stream rejection before commit, no alternate target; forwarding",
		"model", pp.target.Model, "provider", pp.target.Provider, "held_bytes", len(acc))
	return p.flushBootstrapPrefix(pp, acc)
}

// resolveBootstrapRefusal handles a refusal/safeguard terminal detected in the
// buffered head before any client-visible output (NENYA-191): a policy
// outcome, not a provider fault. With an alternate target it fails over
// WITHOUT charging cooldown/breaker — the next target may serve the content.
// On the last target the refusal stream is forwarded: the client must see the
// refusal, and a post-content refusal would already have flushed as output.
func (p *Proxy) resolveBootstrapRefusal(pp bootstrapProbeParams, acc []byte, start time.Time) *streamResult {
	pp.gw.Metrics.RecordBootstrapHold(pp.target.Model, pp.target.Provider, time.Since(start))
	if pp.opts.idx+1 < len(pp.opts.targets) {
		pp.gw.Metrics.RecordStreamBootstrap(pp.target.Model, pp.target.Provider, "refusal_failover")
		pp.gw.Metrics.RecordEarlyStreamError(pp.target.Model, pp.target.Provider, "refusal_failover")
		pp.gw.Logger.Warn("policy refusal at stream head, failing over without breaker charge",
			"model", pp.target.Model, "provider", pp.target.Provider, "held_bytes", len(acc))
		return p.failoverHeadPolicy(pp, streamFailoverPolicyRefusal)
	}
	pp.gw.Metrics.RecordStreamBootstrap(pp.target.Model, pp.target.Provider, "refusal_forwarded")
	pp.gw.Metrics.RecordEarlyStreamError(pp.target.Model, pp.target.Provider, "refusal_forwarded")
	pp.gw.Logger.Warn("policy refusal at stream head, no alternate target; forwarding",
		"model", pp.target.Model, "provider", pp.target.Provider, "held_bytes", len(acc))
	return p.flushBootstrapPrefix(pp, acc)
}

// failoverHeadPolicy closes the upstream attempt and re-arms the retry loop
// for the next target WITHOUT a breaker/cooldown charge (NENYA-191): a policy
// refusal is the model declining the content, so it must never be
// misattributed to provider health. The half-open probe slot consumed by the
// pre-dispatch guard is released explicitly — with no circuit outcome
// recorded, only a release prevents the probe budget from wedging. The
// failover reason is carried so the loop can fail over under a lenient
// sticky_provider (a policy outcome is not a stream fault) and log the
// diagnosis correctly.
func (p *Proxy) failoverHeadPolicy(pp bootstrapProbeParams, reason streamFailoverReason) *streamResult {
	if err := pp.action.resp.Body.Close(); err != nil {
		pp.gw.Logger.Debug("close errored upstream body", "err", err, "provider", pp.target.Provider)
	}
	if pp.stallR != nil {
		pp.stallR.Stop()
		pp.stallR.DrainBuffered()
	}
	pp.action.cancel()
	pp.gw.AgentState.CB.ReleaseHalfOpen(pp.target.CoolKey)
	return &streamResult{empty: true, failoverReason: reason}
}

// flushBootstrapPrefix re-arms the response body so the bytes held during the
// bootstrap probe are served before the (stall-bounded) reader continues.
func (p *Proxy) flushBootstrapPrefix(pp bootstrapProbeParams, acc []byte) *streamResult {
	bodyReader := pp.action.resp.Body
	if pp.stallR != nil {
		bodyReader = pp.stallR
	}
	pp.action.resp.Body = &prefixedReadCloser{prefix: acc, reader: bodyReader}
	return nil
}

// failoverEarlyHeadError fails over before any client byte is committed: the
// upstream body is closed and the retry loop dispatches the next target via
// streamResult{empty}. outcome is the metric reason ("failover" for the
// first-event probe, "bootstrap_failover" for the bootstrap buffer).
func (p *Proxy) failoverEarlyHeadError(gw *gateway.NenyaGateway, action upstreamAction, target routing.UpstreamTarget, cooldownDuration time.Duration, stallR *stallReader, outcome string) *streamResult {
	if err := action.resp.Body.Close(); err != nil {
		gw.Logger.Debug("close errored upstream body", "err", err, "provider", target.Provider)
	}
	if stallR != nil {
		stallR.Stop()
		stallR.DrainBuffered()
	}
	action.cancel()
	gw.AgentState.RecordFailure(target, cooldownDuration)
	gw.Metrics.RecordEarlyStreamError(target.Model, target.Provider, outcome)
	gw.Logger.Warn("upstream error event at stream head, falling back to next target",
		"model", target.Model, "provider", target.Provider)
	return &streamResult{empty: true}
}

// headProbe reports which stream-head probes are enabled by governance config.
type headProbe struct {
	enabled    bool
	empty      bool
	earlyError bool
}

// headProbeEnabled computes the enabled stream-head probe flags. Probes only
// run when at least one of the governing config flags is enabled; the
// always-on streaming empty-200 signal is carried by the SSE reader callback
// instead (NENYA-160), which needs no probe.
func headProbeEnabled(gc config.GovernanceConfig) headProbe {
	hp := headProbe{}
	if gc.EmptyStreamAsError != nil && *gc.EmptyStreamAsError {
		hp.empty = true
	}
	if gc.EarlyStreamErrorFailover != nil && *gc.EarlyStreamErrorFailover {
		hp.earlyError = true
	}
	hp.enabled = hp.empty || hp.earlyError
	return hp
}

// streamResponse handles streaming responses from upstream providers.
// It sets up SSE transformation, monitors for stalls, and streams the response to the client.
func (p *Proxy) streamResponse(opts streamResponseOpts, action upstreamAction) streamResult {
	gw, w, r, target := opts.gw, opts.w, opts.r, opts.target
	cacheKey, cooldownDuration, payload := opts.cacheKey, opts.cooldown, opts.payload
	defer action.cancel()

	// Build the stall-bounded reader once, before the pre-header probe, so the
	// head read is subject to stream_idle_timeout_seconds. The same reader is
	// reused by the streaming phase (byte-preserving: read-ahead buffered by
	// its goroutine stays in line).
	var stallR *stallReader
	if timeout := resolveStreamIdleTimeout(gw, target.Provider); timeout > 0 {
		stallR = newStallReader(r.Context(), action.resp.Body, timeout, gw.Config.Governance.EffectiveThinkingStreamIdleTimeout())
	}

	if res := p.probeStreamHead(gw, action, target, cooldownDuration, opts, stallR); res != nil {
		return *res
	}

	routing.CopyHeaders(action.resp.Header, w.Header())
	if cacheKey != "" {
		w.Header().Set("X-Nenya-Cache-Status", "MISS")
	}
	w.WriteHeader(action.resp.StatusCode)

	extractQuotaUnlessBYOK(gw, r.Context(), target, action.resp.Header)

	buf := getStreamBuffer()
	flushWriter, canFlush := newImmediateFlushWriter(w, resolveStreamStallTimeout(gw))
	dst, captureBuf, tee := p.setupStreamWriter(gw, flushWriter, canFlush, w, cacheKey)

	// Transparent stream continuation: when the upstream stream is cut
	// mid-generation (content seen, no finish_reason, no [DONE]), re-dispatch
	// the same target with the partial assistant message appended so the client
	// sees the stream keep flowing instead of a gateway_error.
	cont := p.newStreamContinuation(gw, opts)
	if cont != nil {
		contTee := &continuationTee{dst: dst, capture: cont.capture}
		dst = contTee
	}

	maxAttempts := 1
	if cont != nil {
		maxAttempts = cont.maxAttempts
	}

	// One slow-call signal per logical request (NENYA-153): only the first
	// attempt measures the first token, and the terminal outcome is recorded
	// once, after the last attempt. The upstream-error verdict is shared the
	// same way.
	slowCall := newSlowCallSignal(slowCallThresholdFor(gw, opts.agentName))
	upstreamErr := &upstreamErrorState{}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if cont != nil {
			cont.capture.reset()
		}

		// Continuation attempts re-dispatch with a fresh upstream response, so
		// they must build their own stall reader around the new body. Only the
		// first attempt reuses the pre-probe reader (byte-preserving).
		attemptStall := stallR
		if attempt > 0 {
			attemptStall = nil
		}
		// NENYA-189: scope the thought-signature view to the caller's API
		// key; a nil key fails closed (nil view = no cached signature state).
		sigCache := scopedThoughtSigCache(gw, opts.apiKey)
		transformingReader, contentBuilder, stallR := p.setupTransformingReader(gw, transformReaderOpts{
			target:       target,
			agentName:    opts.agentName,
			sourceFormat: opts.sourceFormat,
			action:       action,
			ctx:          r.Context(),
			reuse:        attemptStall,
			canary:       opts.canary,
			sigCache:     sigCache,
			dispatchAt:   ttftDispatchAtFor(attempt, opts.dispatchAt),
			slowCall:     slowCall,
			upstreamErr:  upstreamErr,
		})
		if transformingReader == nil {
			// Defensive: setupTransformingReader does not return nil today. If
			// it ever did, the dispatch consumed a half-open probe slot and no
			// outcome is recorded, so release it.
			gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
			putStreamBuffer(buf)
			return streamResult{}
		}

		// On all but the final continuation attempt, suppress the cut-error
		// injection so the stream can end with a plain EOF and be resumed. On
		// the final attempt we keep the default behavior (inject gateway_error).
		if cont != nil {
			transformingReader.SetSuppressCutError(attempt+1 < maxAttempts)
		}

		_, copyErr := copyStream(r.Context(), dst, transformingReader, *buf)

		if isStreamCut(cont, transformingReader, copyErr) && attempt+1 < maxAttempts {
			status, newAction := p.tryContinueStream(gw, w, r, opts, target, payload, cooldownDuration, action, cont, stallR, buf)
			if status == streamContinueOK {
				// The abandoned attempt's dispatch consumed a half-open probe
				// slot and no outcome is recorded for it: release it, or a
				// continuation leaks one slot per resume (NENYA-153).
				gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
				action = newAction
				// The action swap happened for this attempt; the new action's
				// http response carries the continuation's StatusCode, which is
				// equal to the original (both should be 200). Ensure the
				// deferred cancel on streamResponse doesn't leak the previous
				// action's context.
				defer action.cancel()
				continue
			}
			// The re-dispatch's slot is resolved by resolveRedispatchOutcome
			// inside tryContinueStream; the original attempt recorded no outcome,
			// so release its slot here.
			gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
			return streamResult{}
		}

		// Final attempt: on a suppressed (non-continuable) cut the reader
		// already injected its terminator, so finalize without polluting the
		// response cache with the partial cut stream.
		if isStreamCut(cont, transformingReader, copyErr) {
			gw.Metrics.RecordStreamContinuation(target.Model, target.Provider, "gave_up_exhausted")
			return p.handleStreamDone(gw, w, target, opts.agentName, action, "", cooldownDuration, payload, buf, copyErr, nil, nil, contentBuilder, stallR, r.Context(), terminalSignals{injectedErr: transformingReader.InjectedError(), slowCall: slowCall, upstreamErr: upstreamErr, resumable: transformingReader.SawResumableStop()}, opts.apiKey)
		}

		return p.handleStreamDone(gw, w, target, opts.agentName, action, cacheKey, cooldownDuration, payload, buf, copyErr, captureBuf, tee, contentBuilder, stallR, r.Context(), terminalSignals{injectedErr: transformingReader.InjectedError(), slowCall: slowCall, upstreamErr: upstreamErr, resumable: transformingReader.SawResumableStop()}, opts.apiKey)
	}

	putStreamBuffer(buf)
	return streamResult{}
}

// resolveTransformer selects the appropriate SSE transformer based on source format and target format.
func (p *Proxy) resolveTransformer(gw *gateway.NenyaGateway, target routing.UpstreamTarget, sourceFormat string, sigCache infra.ThoughtSignatureKV) stream.ResponseTransformer {
	if sourceFormat == "anthropic" && target.Format != "anthropic" {
		// NENYA-51: run the provider transformer FIRST (it caches Gemini
		// thought signatures on the upstream's native OpenAI format), then
		// the OpenAI→Anthropic converter for the client. Short-circuiting to
		// the converter alone dropped every signature for Anthropic-source
		// clients on Gemini upstreams.
		reverse := stream.NewOpenAIToAnthropicTransformer()
		if spec, ok := providerpkg.Get(target.Provider); ok && spec.NewResponseTransformer != nil {
			if provider := spec.NewResponseTransformer(sigCache); provider != nil {
				gw.Logger.Debug("SSE composite transformer active (provider cache + reverse conversion)",
					"provider", target.Provider)
				return &compositeTransformer{providerFirst: provider, clientSecond: reverse}
			}
		}
		gw.Logger.Debug("SSE reverse transformer active (Anthropic client, OpenAI upstream)", "provider", target.Provider)
		return reverse
	}

	if target.Format == "anthropic" {
		if sourceFormat == "anthropic" {
			// Anthropic-native client, Anthropic-format target (NENYA-176):
			// the upstream's SSE is already the client's wire shape — pass
			// it through instead of translating it into OpenAI chunks
			// (mirrors the buffered gate at chat.go).
			gw.Logger.Debug("SSE passthrough (Anthropic client, Anthropic upstream)", "provider", target.Provider)
			return nil
		}
		gw.Logger.Debug("SSE transformer active", "provider", target.Provider, "format", target.Format)
		return stream.NewAnthropicTransformer()
	}

	if spec, ok := providerpkg.Get(target.Provider); ok && spec.NewResponseTransformer != nil {
		transformer := spec.NewResponseTransformer(sigCache)
		if transformer != nil {
			gw.Logger.Debug("SSE transformer active", "provider", target.Provider)
		}
		return transformer
	}

	return nil
}

// resolveStreamStallTimeout returns the per-write client-stall deadline
// (NENYA-155): an explicit 0 disables, anything positive is seconds. A nil
// field or nil gateway also disables — ApplyDefaults forces the 60s default
// at load/reload, so this path is reachable only from programmatically built
// configs (tests).
func resolveStreamStallTimeout(gw *gateway.NenyaGateway) time.Duration {
	if gw == nil {
		return 0
	}
	if v := gw.Config.Server.StreamStallTimeoutSeconds; v != nil && *v > 0 {
		return time.Duration(*v) * time.Second
	}
	return 0
}

func resolveStreamIdleTimeout(gw *gateway.NenyaGateway, providerName string) time.Duration {
	timeout := gw.Config.Governance.EffectiveStreamIdleTimeout()
	if provider, ok := gw.Providers[providerName]; ok && provider.StreamIdleTimeoutSeconds > 0 {
		providerTimeout := time.Duration(provider.StreamIdleTimeoutSeconds) * time.Second
		if providerTimeout > 86400*time.Second {
			providerTimeout = 86400 * time.Second
		}
		return providerTimeout
	}
	return timeout
}

// resolveBootstrapBuffer returns the stream bootstrap buffering budget for a
// provider: the per-provider opt-in when set, else the governance global.
// 0 means disabled; validation rejects negatives, but stay defensive.
func resolveBootstrapBuffer(gw *gateway.NenyaGateway, providerName string) int {
	if pr, ok := gw.Providers[providerName]; ok && pr.StreamBootstrapBufferBytes != nil {
		if *pr.StreamBootstrapBufferBytes > 0 {
			return *pr.StreamBootstrapBufferBytes
		}
		return 0
	}
	if gw.Config.Governance.StreamBootstrapBufferBytes > 0 {
		return gw.Config.Governance.StreamBootstrapBufferBytes
	}
	return 0
}

// transformReaderOpts groups the per-request inputs for
// setupTransformingReader.
type transformReaderOpts struct {
	target       routing.UpstreamTarget
	agentName    string
	sourceFormat string
	action       upstreamAction
	ctx          context.Context
	reuse        *stallReader
	canary       pipeline.CanarResult
	sigCache     infra.ThoughtSignatureKV
	// dispatchAt starts the TTFT clock (NENYA-178).
	dispatchAt time.Time
	// slowCall receives the measured first-token latency of the logical request
	// so an over-threshold one can be charged to the breaker (NENYA-153). Nil
	// when the threshold is off.
	slowCall *slowCallSignal
	// upstreamErr receives the in-stream error verdict (NENYA-153) so the
	// terminal outcome cannot clear a failure the observer already charged.
	upstreamErr *upstreamErrorState
}

// setupTransformingReader creates and configures the SSE transforming reader,
// content builder, and stall reader. It returns the transforming reader for
// streaming, the content builder for post-processing, and the stall reader for
// cleanup. When the stream idle timeout is disabled, stallR is nil and the
// body passes straight to the transforming reader. o.reuse, when non-nil, is
// a stall reader already built around this body (by the stream-head probe);
// it is used as-is so its buffered read-ahead bytes stay in line and
// backgroundExhaust is not double-allocated.
func (p *Proxy) setupTransformingReader(gw *gateway.NenyaGateway, o transformReaderOpts) (*stream.SSETransformingReader, *contentBuilder, *stallReader) {
	transformer := p.resolveTransformer(gw, o.target, o.sourceFormat, o.sigCache)

	var bodyReader io.Reader = o.action.resp.Body
	var stallR = o.reuse
	if stallR == nil {
		timeout := resolveStreamIdleTimeout(gw, o.target.Provider)
		if timeout > 0 {
			thinkingTimeout := gw.Config.Governance.EffectiveThinkingStreamIdleTimeout()
			stallR = newStallReader(o.ctx, o.action.resp.Body, timeout, thinkingTimeout)
			bodyReader = stallR
		}
	} else if _, wrapped := o.action.resp.Body.(*prefixedReadCloser); wrapped {
		// The stream-head probe wrapped the body (prefix over the reused stall
		// reader): keep reading through the wrapper so byte order is preserved.
		bodyReader = o.action.resp.Body
	} else {
		// Probe disabled: the reused stall reader's goroutine is the sole
		// consumer of the raw body. Reading the raw body directly here would
		// race it and starve the stream, so read through the stall reader.
		bodyReader = stallR
	}

	transformingReader := stream.NewSSETransformingReader(bodyReader, transformer, o.ctx)
	// NENYA-160: the always-on streaming empty-200 signal rides the reader's
	// own empty-stream handler, so it fires regardless of the probe flags.
	transformingReader.SetEmptyStreamCallback(func() {
		gw.Metrics.RecordEmptyResponse(o.target.Model, o.target.Provider, "stream")
	})
	transformingReader.SetDropPrivateEvents(gw.Config.Governance.FilterPrivateSSEEvents)
	transformingReader.SetMaxTransformedBytes(gw.Config.Governance.EffectiveMaxTransformedSSEBytes())
	transformingReader.SetOnUsage(p.makeUsageCallback(o.ctx, gw, o.target, o.agentName))
	observer := newUpstreamErrorObserver(gw, o.target)
	observer.state = o.upstreamErr
	transformingReader.SetObserver(observer)
	transformingReader.SetLogger(gw.Logger)

	p.setupStreamFilterIfEnabled(gw, transformingReader)
	if guard := exfilGuardFor(gw, o.agentName); guard != nil {
		transformingReader.SetExfilGuard(guard)
	}
	if o.canary.Token != "" {
		transformingReader.SetCanaryWatcher(stream.NewCanaryWatcher(o.canary.Token, o.canary.Action, "stream", gw.Metrics, gw.Logger.With("agent", o.agentName)))
	}
	p.setupStreamEntropyFilterIfEnabled(gw, transformingReader)
	contentBuilder := p.setupContentBuilderIfNeeded(gw, o.agentName, transformingReader)

	if stallR != nil {
		transformingReader.SetOnThinking(func(active bool) {
			stallR.SetThinkingActive(active)
		})
	}

	setupTTFTRecorder(gw, o, transformingReader)

	return transformingReader, contentBuilder, stallR
}

// ttftDispatchAtFor returns the TTFT clock start for one streaming attempt
// (NENYA-178): the dispatch instant on the first attempt, and the zero time
// afterwards. A continuation re-dispatches mid-generation, so measuring its
// first output would double-count the logical request's time-to-first-token
// (inflated by the whole cut stream) and skew the slow-call breaker. A retried
// or failed-over target gets its own streamResponse call and its own
// measurement, which is what per-target TTFT should mean.
//
// Consequence: a first attempt cut after reasoning-only frames records no
// sample at all (the cut is recognized from reasoning content, but the default
// clock does not stop on reasoning). That is a deliberate no-sample outcome —
// the alternative is an inflated measurement.
func ttftDispatchAtFor(attempt int, dispatchAt time.Time) time.Time {
	if attempt > 0 {
		return time.Time{}
	}
	return dispatchAt
}

// setupTTFTRecorder wires the time-to-first-effective-output clock (NENYA-178)
// for one streaming attempt: the reader fires a one-shot callback at the first
// output-bearing frame (defined by stream.HasEffectiveOutput — never a keepalive,
// an empty delta, a block close or a role-only handshake; reasoning frames only
// when the operator opts in), and the elapsed time since the dispatch is recorded
// per (model, provider) for /statsz and as nenya_ttft_seconds. A zero dispatch
// instant (see ttftDispatchAtFor) makes it a no-op; a nil tracker still observes
// the signal when a slow-call threshold is configured.
func setupTTFTRecorder(gw *gateway.NenyaGateway, o transformReaderOpts, r *stream.SSETransformingReader) {
	if gw == nil || (gw.LatencyTracker == nil && o.slowCall == nil) {
		// Nothing to observe: no tracker to record into and no slow-call
		// threshold to judge against.
		return
	}
	if o.dispatchAt.IsZero() {
		// A zero instant means "do not measure this attempt" (a continuation
		// re-dispatch, see ttftDispatchAtFor). Log it so a future caller that
		// forgets to plumb the dispatch instant loses the signal loudly rather
		// than silently.
		if gw.Logger != nil {
			gw.Logger.Debug("skipping TTFT measurement: no dispatch instant",
				"model", o.target.Model, "provider", o.target.Provider)
		}
		return
	}
	target, start := o.target, o.dispatchAt
	includeReasoning := gw.Config.Governance.TTFTIncludeReasoningFrames()
	signal := o.slowCall
	tracker := gw.LatencyTracker
	r.SetOnFirstEffectiveOutput(func() {
		elapsed := time.Since(start)
		signal.observe(elapsed)
		if tracker == nil {
			return
		}
		tracker.RecordTTFT(target.Model, target.Provider, elapsed)
		gw.Metrics.RecordTTFT(target.Model, o.agentName, target.Provider, elapsed)
		if gw.Logger != nil {
			gw.Logger.Debug("recorded time to first effective output",
				"model", target.Model, "provider", target.Provider, "ttft_ms", elapsed.Milliseconds())
		}
	}, includeReasoning)
}

// upstreamErrorState records that a stream carried an upstream error event
// (NENYA-153): the observer charges that failure while the frame is parsed, and
// the terminal outcome reads this flag so it does not clear the charge with a
// success. Written and read on the goroutine that drains the stream.
type upstreamErrorState struct{ seen bool }

// upstreamErrorObserver is an SSE observer that detects error events within
// a 200 OK stream and records them as circuit breaker failures.
type upstreamErrorObserver struct {
	gw     *gateway.NenyaGateway
	target routing.UpstreamTarget
	state  *upstreamErrorState
}

// newUpstreamErrorObserver creates an SSE observer that detects error events
// within a 200 OK stream and records them as circuit breaker failures.
func newUpstreamErrorObserver(gw *gateway.NenyaGateway, target routing.UpstreamTarget) *upstreamErrorObserver {
	return &upstreamErrorObserver{gw: gw, target: target}
}

func (o *upstreamErrorObserver) OnSSEEvent(event stream.SSEEvent) {
	if event.Type != "error" || o.gw == nil {
		return
	}
	if stream.IsPolicyRefusalPayload(event.Data) {
		// A content-policy/safeguard rejection (NENYA-191) is the model
		// declining the content, not a provider fault: no breaker charge, and
		// the terminal outcome may still record the provider's success.
		o.gw.Logger.Warn("policy rejection event in stream, not charged to the breaker",
			"model", o.target.Model, "provider", o.target.Provider)
		return
	}
	if event.GatewayInjected {
		// Errors synthesized by the gateway (e.g. a gateway_error injected
		// when an upstream stream ends without [DONE]) are not provider
		// failures and must not count toward the circuit breaker.
		o.gw.Logger.Debug("ignoring gateway-injected error event for circuit breaker",
			"model", o.target.Model, "provider", o.target.Provider)
		return
	}
	if o.state != nil {
		o.state.seen = true
	}
	errData := event.Data["error"]
	if errData == nil {
		o.gw.Logger.Warn("upstream error event detected in stream (malformed, missing 'error' field)",
			"model", o.target.Model, "provider", o.target.Provider,
			"event_data", event.Data)
		o.gw.AgentState.RecordFailure(o.target, 0)
		return
	}
	if errMap, ok := errData.(map[string]any); ok {
		errCode := ""
		for _, key := range []string{"code", "type", "error_code"} {
			if v, exists := errMap[key]; exists && v != nil {
				errCode = fmt.Sprintf("%v", v)
				break
			}
		}
		logAttrs := []any{
			"model", o.target.Model,
			"provider", o.target.Provider,
			"error_type", fmt.Sprintf("%v", errMap["type"]),
			"error_message", fmt.Sprintf("%v", errMap["message"]),
		}
		if errCode != "" {
			logAttrs = append(logAttrs, "error_code", errCode)
		}
		o.gw.Logger.Warn("upstream error event detected in stream", logAttrs...)
	} else {
		o.gw.Logger.Warn("upstream error event detected in stream (malformed 'error' field)",
			"model", o.target.Model, "provider", o.target.Provider,
			"error_data", errData)
	}
	o.gw.AgentState.RecordFailure(o.target, 0)
}

// OnStreamClose is called when the SSE stream closes.
func (o *upstreamErrorObserver) OnStreamClose(err error) {}

func (p *Proxy) setupStreamFilterIfEnabled(gw *gateway.NenyaGateway, r *stream.SSETransformingReader) {
	if !gw.Config.Bouncer.RedactOutput {
		return
	}
	if len(gw.SecretPatterns) == 0 && len(gw.BlockedPatterns) == 0 {
		return
	}
	sf := stream.NewStreamFilter(gw.SecretPatterns, gw.BlockedPatterns, gw.Config.Bouncer.RedactionLabel, gw.Config.Bouncer.RedactOutputWindow)
	r.SetStreamFilter(sf)
	gw.Logger.Debug("stream filter active",
		"secret_patterns", len(gw.SecretPatterns),
		"block_patterns", len(gw.BlockedPatterns),
		"window_size", gw.Config.Bouncer.RedactOutputWindow)
}

func (p *Proxy) setupStreamEntropyFilterIfEnabled(gw *gateway.NenyaGateway, r *stream.SSETransformingReader) {
	if gw.EntropyFilter == nil || !gw.Config.Bouncer.RedactOutput {
		return
	}
	ef := stream.NewStreamEntropyFilter(
		gw.EntropyFilter.RedactHighEntropy,
		gw.Config.Bouncer.RedactionLabel,
		gw.Config.Bouncer.RedactOutputWindow,
	)
	r.SetStreamEntropyFilter(ef)
	gw.Logger.Debug("stream entropy filter active",
		"threshold", gw.Config.Bouncer.EntropyThreshold,
		"min_token", gw.Config.Bouncer.EntropyMinToken,
		"window_size", gw.Config.Bouncer.RedactOutputWindow)
}

func (p *Proxy) setupContentBuilderIfNeeded(gw *gateway.NenyaGateway, agentName string, r *stream.SSETransformingReader) *contentBuilder {
	agent, ok := gw.Config.Agents[agentName]
	if !ok || agent.MCP == nil || !agent.MCP.AutoSave {
		return nil
	}
	cb := newContentBuilder()
	r.SetOnContent(cb.addContent)
	return cb
}

func (p *Proxy) setupStreamWriter(gw *gateway.NenyaGateway, flushWriter *immediateFlushWriter, canFlush bool, w http.ResponseWriter, cacheKey string) (io.Writer, *bytes.Buffer, *sseTeeWriter) {
	dst := io.Writer(flushWriter)
	if !canFlush {
		dst = w
	}

	var captureBuf *bytes.Buffer
	var tee *sseTeeWriter
	if cacheKey != "" && gw.ResponseCache != nil {
		captureBuf = new(bytes.Buffer)
		tee = &sseTeeWriter{
			dst:      flushWriter,
			buf:      captureBuf,
			maxBytes: gw.Config.ResponseCache.MaxEntryBytes,
		}
		dst = tee
	}

	return dst, captureBuf, tee
}

// terminalSignals groups the one-off terminal flags of a streamed attempt, so
// handleStreamDone's positional list does not grow one bool per policy.
type terminalSignals struct {
	// resumable (NENYA-174): the reader saw a resumable stop reason
	// (pause_turn) on the raw surface — the capture is not a complete
	// answer even though the transformed frames say "stop".
	resumable bool
	// injectedErr marks a stream the reader terminated with a synthesized
	// gateway_error.
	injectedErr bool
	// slowCall carries the logical request's first-token measurement
	// (NENYA-153); nil when the slow-call threshold is off.
	slowCall *slowCallSignal
	// upstreamErr carries the in-stream error verdict (NENYA-153): the observer
	// already charged that failure, so the terminal outcome must not clear it.
	upstreamErr *upstreamErrorState
}

func (p *Proxy) handleStreamDone(gw *gateway.NenyaGateway, w http.ResponseWriter, target routing.UpstreamTarget, agentName string, action upstreamAction, cacheKey string, cooldownDuration time.Duration, payload map[string]any, buf *[]byte, copyErr error, captureBuf *bytes.Buffer, tee *sseTeeWriter, contentBuilder *contentBuilder, stallR *stallReader, reqCtx context.Context, terminal terminalSignals, apiKey *config.ApiKey) streamResult {
	putStreamBuffer(buf)

	if handled, result := p.handleBlockedStream(gw, w, target, copyErr, action, stallR); handled {
		return result
	}

	// Close upstream body and cancel context before draining to unblock
	// the readLoop, preventing DrainPending from blocking for the full timeout.
	action.cancel()
	_ = action.resp.Body.Close()

	if stallR != nil {
		drained, err := stallR.DrainPending(100 * time.Millisecond)
		if err != nil {
			gw.Logger.Debug("stall reader drain result", "drained_bytes", drained, "err", err)
		}
	}

	if errors.Is(copyErr, errStreamStalled) {
		gw.AgentState.RecordFailure(target, cooldownDuration)
		gw.Logger.Warn("stream stalled, terminating SSE (terminal)",
			"agent", agentName, "model", target.Model, "provider", target.Provider,
			"idle_timeout", gw.Config.Governance.EffectiveStreamIdleTimeout())
		gw.Metrics.RecordStreamStall(target.Model, target.Provider)
		p.writeStallSSE(gw, w)
		// Headers and partial SSE are already committed: terminal, never
		// fail over and append a second stream to the client's bytes.
		return streamResult{terminal: true}
	}

	if isClientWriteError(copyErr) {
		stalled := isClientStalled(copyErr)
		if stalled {
			// NENYA-155: the peer stopped reading — the per-write deadline
			// fired. Classified client_stalled: no provider cooldown or
			// breaker charge, only telemetry.
			gw.Metrics.RecordClientStalled(target.Model, target.Provider)
		}
		gw.Logger.Info("client disconnected mid-write, aborting upstream stream",
			"model", target.Model, "provider", target.Provider,
			"client_stalled", stalled)
		// Connection-scoped (NENYA-42): no breaker outcome, but the dispatch
		// consumed a half-open probe slot, and a release is not a state write.
		gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
		return streamResult{}
	}

	if errors.Is(copyErr, context.Canceled) {
		gw.Logger.Info("client disconnected, aborting upstream stream",
			"model", target.Model, "provider", target.Provider)
	} else if errors.Is(copyErr, context.DeadlineExceeded) {
		gw.Logger.Warn("upstream timeout reached, sending terminator SSE",
			"model", target.Model, "provider", target.Provider,
			"timeout", gw.Config.Governance.EffectiveUpstreamTimeout())
		gw.Metrics.RecordStreamInterrupt(target.Model, target.Provider, "timeout")
		p.writeTimeoutSSE(gw, w)
	}

	if terminal.injectedErr && copyErr == nil {
		// NENYA-27: the reader sanitized a truncated/empty upstream stream
		// with a synthesized gateway_error terminal. The exchange is a
		// provider failure (cooldown the target) even though the copy loop
		// saw a clean EOF, and the truncated output must never reach the
		// response cache or the MCP auto-save below.
		gw.Logger.Warn("stream truncated, sanitized with injected gateway_error terminal",
			"model", target.Model, "provider", target.Provider)
		gw.AgentState.RecordFailure(target, cooldownDuration)
		gw.Metrics.RecordStreamInterrupt(target.Model, target.Provider, "truncated")
		return streamResult{}
	}

	recordStreamResult(gw, streamOutcome{
		target:           target,
		agentName:        agentName,
		cooldownDuration: cooldownDuration,
		copyErr:          copyErr,
		slowCall:         terminal.slowCall,
		upstreamErr:      terminal.upstreamErr,
	})

	if copyErr == nil {
		storeStreamCache(gw, cacheKey, captureBuf, tee, payload, reqCtx, terminal.resumable)
		p.asyncMCPAutoSave(gw, agentName, contentBuilder, apiKey)
		return streamResult{}
	}
	if errors.Is(copyErr, context.Canceled) || errors.Is(copyErr, context.DeadlineExceeded) {
		return streamResult{}
	}
	writeSSEError(gw, w, http.StatusOK, "upstream stream interrupted")
	return streamResult{}
}

func streamErrorReason(err error) string {
	if err == nil {
		return "success"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "upstream timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "client disconnect"
	}
	return "unknown"
}

// streamOutcome carries the terminal signals that classify a streamed attempt.
type streamOutcome struct {
	target           routing.UpstreamTarget
	agentName        string
	cooldownDuration time.Duration
	copyErr          error
	// slowCall carries the logical request's first-token measurement (NENYA-153);
	// nil when the slow-call threshold is off.
	slowCall *slowCallSignal
	// upstreamErr reports that the stream carried an upstream error event: the
	// observer already charged that failure, so the terminal outcome must not
	// clear it with a success.
	upstreamErr *upstreamErrorState
}

// recordStreamResult records the outcome of a streaming request to the agent
// state and metrics. A served stream whose first effective output exceeded the
// agent's slow-call threshold is charged as a breaker failure instead of a
// success (NENYA-153): the client keeps its response, the breaker sees the
// degradation.
func recordStreamResult(gw *gateway.NenyaGateway, o streamOutcome) {
	if o.target.CoolKey == "" {
		return
	}
	if o.copyErr == nil {
		// An in-stream error event was already charged by the observer while the
		// frame was parsed; recording a success here would clear it (and close a
		// half-open circuit on a stream the client saw fail).
		if o.upstreamErr != nil && o.upstreamErr.seen {
			return
		}
		if o.slowCall.slow() {
			// Never fall through to a success once the first token was slow:
			// the success would clear the failure streak (and close a half-open
			// circuit) that this measurement exists to charge.
			recordSlowCallAt(gw, o.target, o.agentName, o.slowCall.threshold, o.slowCall.elapsed, o.cooldownDuration)
			return
		}
		if !o.target.BYOK {
			gw.AgentState.RecordSuccess(o.target.CoolKey)
		}
		return
	}
	// A client-side cancel or a request deadline is connection-scoped
	// (NENYA-42): no state write, matching every sibling cancel path — the
	// client never received the completion, so it must not count as a provider
	// success (which would also close a half-open circuit).
	if errors.Is(o.copyErr, context.Canceled) || errors.Is(o.copyErr, context.DeadlineExceeded) {
		gw.Logger.Debug("stream ended", "model", o.target.Model, "provider", o.target.Provider, "reason", streamErrorReason(o.copyErr))
		gw.AgentState.CB.ReleaseHalfOpen(o.target.CoolKey)
		return
	}
	gw.Logger.Warn("stream copy error (upstream)",
		"model", o.target.Model, "provider", o.target.Provider, "err", o.copyErr)
	gw.AgentState.RecordFailure(o.target, o.cooldownDuration)
}

func storeStreamCache(gw *gateway.NenyaGateway, cacheKey string, captureBuf *bytes.Buffer, tee *sseTeeWriter, payload map[string]any, reqCtx context.Context, resumable bool) {
	if cacheKey == "" || gw.ResponseCache == nil || tee == nil || tee.exceeded || captureBuf.Len() <= 0 {
		return
	}

	captured := captureBuf.Bytes()

	// Skip caching for completions that terminated with refusal/content_filter
	// outcomes (decoded from the terminal events, not substring-matched —
	// NENYA-27), for network-failure finish_reasons, and for streams whose
	// terminal event is an upstream error object. Reader-injected truncation
	// terminals never reach this function (handleStreamDone skips earlier).
	if capturedStreamHasRefusalOrFilter(captured) || capturedStreamHasNetworkErrorFinish(captured) ||
		capturedStreamEndsWithErrorObject(captured) {
		return
	}
	if capturedStreamHasResumableTerminal(captured) || resumable {
		// pause_turn-class terminals (NENYA-174): the upstream expects a
		// resume, so the captured prefix is not a complete answer — caching
		// it would serve a truncated turn on a later identical request.
		// terminal.resumable covers CONVERTED captures (the transformer
		// erases the raw reason before the capture tee sees it); the byte
		// scan covers native passthrough.
		return
	}

	var embedding []float32
	if gw.Config.ResponseCache.EnableSemantic && payload != nil {
		embedding = computeEmbedding(gw, payload, reqCtx)
	}

	// NENYA-189: the semantic namespace's model segment must be the
	// client-requested identifier (payload["model"] — the same value mixed
	// into the cache fingerprint and used by Lookup), NOT target.Model: for
	// agent routes the alias and the resolved upstream model differ, and a
	// mismatch would silently disable semantic hits.
	modelName, _ := payload["model"].(string)
	gw.ResponseCache.Store(cacheKey, modelName, true, captured, embedding)
	gw.Logger.Debug("response cache stored", "size", len(captured), "has_embedding", embedding != nil)
}

func computeEmbedding(gw *gateway.NenyaGateway, payload map[string]any, reqCtx context.Context) []float32 {
	if !gw.Config.ResponseCache.EnableSemantic {
		return nil
	}

	text := extractUserTextForEmbedding(payload)
	if text == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(reqCtx, 10*time.Second)
	defer cancel()

	start := time.Now()
	embedding, err := gw.Embedder.Embed(ctx, text)
	duration := time.Since(start)

	if gw.Metrics != nil {
		gw.Metrics.RecordEmbeddingDuration(duration)
		if err != nil {
			gw.Metrics.RecordEmbeddingError("ollama")
		}
	}

	if err != nil {
		gw.Logger.Debug("failed to compute embedding for cache store", "error", err)
		return nil
	}
	return embedding
}

func extractUserTextForEmbedding(payload map[string]any) string {
	const maxEmbeddingTextLen = 10000

	messagesRaw, ok := payload["messages"]
	if !ok {
		return ""
	}
	messages, ok := messagesRaw.([]any)
	if !ok || len(messages) == 0 {
		return ""
	}

	var userMsgs strings.Builder
	estimatedLen := len(messages) * 100
	if estimatedLen > maxEmbeddingTextLen {
		estimatedLen = maxEmbeddingTextLen
	}
	userMsgs.Grow(estimatedLen)

	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok || role != "user" {
			continue
		}
		content, ok := msg["content"].(string)
		if !ok {
			continue
		}
		if userMsgs.Len()+len(content) > maxEmbeddingTextLen {
			break
		}
		userMsgs.WriteString(content)
		userMsgs.WriteString("\n")
	}

	return userMsgs.String()
}

// writeStallSSE sends a gateway_error SSE event followed by [DONE] to the
// client. This is used when the upstream stream stalls (no data within the
// idle timeout). The error event lets the client distinguish a stall from a
// normal completion.
func (p *Proxy) writeStallSSE(gw *gateway.NenyaGateway, w http.ResponseWriter) {
	armClientWriteDeadline(w, resolveStreamStallTimeout(gw))
	_, _ = w.Write(stallSSEFrames())
	flushPassthrough(w)
}

// stallSSEFrames renders the structured stall terminal: a gateway_error event
// carrying error_kind=provider_timeout (NENYA-156 — Retryable+ShouldFailover,
// matching the cooldown the breaker just took) followed by [DONE]. Shared by
// the chat path and the passthrough copier so the framing cannot drift.
func stallSSEFrames() []byte {
	errPayload, err := json.Marshal(map[string]any{
		"error_kind": string(infra.ErrorKindProviderTimeout),
		"error": map[string]any{
			"message": "upstream stream stalled: no data received within idle timeout",
			"type":    "gateway_error",
		},
	})
	if err != nil {
		errPayload = []byte(`{"error_kind":"provider_timeout","error":{"message":"upstream stream stalled","type":"gateway_error"}}`)
	}
	return []byte(fmt.Sprintf("data: %s\n\ndata: [DONE]\n\n", errPayload))
}

func (p *Proxy) writeTimeoutSSE(gw *gateway.NenyaGateway, w http.ResponseWriter) {
	armClientWriteDeadline(w, resolveStreamStallTimeout(gw))
	errPayload, err := json.Marshal(map[string]any{
		"error_kind": string(infra.ErrorKindProviderTimeout),
		"error": map[string]any{
			"message": "upstream timeout reached",
			"type":    "gateway_error",
		},
	})
	if err != nil {
		gw.Logger.Error("failed to marshal timeout error payload", "err", err)
		errPayload = []byte(`{"error_kind":"provider_timeout","error":{"message":"upstream timeout reached","type":"gateway_error"}}`)
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", errPayload)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// writeBlockedSSE sends a blocked response SSE stream to the client.
// This is used when the execution policy blocks a request.
func (p *Proxy) writeBlockedSSE(gw *gateway.NenyaGateway, w http.ResponseWriter) {
	p.writeBlockedSSEPayload(gw, w, map[string]interface{}{
		"id":     "blocked",
		"object": "chat.completion.chunk",
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{"content": "[Response blocked by execution policy]"},
				"finish_reason": "stop",
			},
		},
	})
}

// handleBlockedStream resolves the three stream-block error kinds
// (canary tripwire, egress guard, execution policy): it kills the
// upstream, drains the stall reader, records the block and writes the
// kind-specific SSE frame. Every handled block is terminal — the frame is
// the whole response. Returns handled=false for other errors.
func (p *Proxy) handleBlockedStream(gw *gateway.NenyaGateway, w http.ResponseWriter, target routing.UpstreamTarget, copyErr error, action upstreamAction, stallR *stallReader) (bool, streamResult) {
	var message string
	var writer func(*gateway.NenyaGateway, http.ResponseWriter)
	switch {
	case errors.Is(copyErr, stream.ErrCanaryDetected):
		message = "stream blocked: canary detected, upstream killed"
		writer = p.writeExfilDetectedSSE
	case errors.Is(copyErr, stream.ErrExfilBlocked):
		message = "stream blocked by exfil guard, upstream killed"
		writer = p.writeExfilBlockedSSE
	case errors.Is(copyErr, stream.ErrStreamBlocked):
		message = "stream blocked by execution policy, upstream killed"
		writer = p.writeBlockedSSE
	default:
		return false, streamResult{}
	}

	action.cancel()
	_ = action.resp.Body.Close()
	if stallR != nil {
		stallR.Stop()
		stallR.DrainBuffered()
	}
	// A policy block is not a provider failure, so the target is deliberately
	// not cooled down (recordStreamResult is never reached). The half-open
	// probe slot consumed by the pre-dispatch guard must still be released:
	// without an outcome or an explicit release, a block on a half-open probe
	// would leak the slot and wedge the circuit permanently.
	gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
	gw.Logger.Warn(message, "model", target.Model, "provider", target.Provider)
	gw.Metrics.RecordStreamBlock(target.Model, target.Provider)
	writer(gw, w)
	// Every branch fully writes the response (a structured block frame):
	// the retry loop must not fail over onto the committed bytes, so all
	// three terminations are terminal.
	return true, streamResult{terminal: true}
}

// writeExfilDetectedSSE sends the structured canary-detection response:
// an error object carrying error_kind=exfil_detected plus a human-
// readable delta, then [DONE].
func (p *Proxy) writeExfilDetectedSSE(gw *gateway.NenyaGateway, w http.ResponseWriter) {
	p.writeBlockedSSEPayload(gw, w, map[string]interface{}{
		"id":         "blocked",
		"object":     "chat.completion.chunk",
		"error_kind": string(infra.ErrorKindExfilDetected),
		"error": map[string]interface{}{
			"message": "response blocked: canary token detected in output",
			"type":    string(infra.ErrorKindExfilDetected),
		},
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{"content": "[Response blocked: canary detected in output]"},
				"finish_reason": "stop",
			},
		},
	})
}

// writeExfilBlockedSSE sends the structured egress-block response to the
// client: an error object carrying error_kind=exfil_blocked plus a
// human-readable delta, then [DONE].
func (p *Proxy) writeExfilBlockedSSE(gw *gateway.NenyaGateway, w http.ResponseWriter) {
	p.writeBlockedSSEPayload(gw, w, map[string]interface{}{
		"id":         "blocked",
		"object":     "chat.completion.chunk",
		"error_kind": string(infra.ErrorKindExfil),
		"error": map[string]interface{}{
			"message": "response blocked by data-exfiltration policy",
			"type":    string(infra.ErrorKindExfil),
		},
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{"content": "[Response blocked: data-exfiltration policy]"},
				"finish_reason": "stop",
			},
		},
	})
}

// writeBlockedSSEPayload marshals a blocked-response payload and writes
// it as one SSE data line plus [DONE].
func (p *Proxy) writeBlockedSSEPayload(gw *gateway.NenyaGateway, w http.ResponseWriter, blockPayload map[string]interface{}) {
	armClientWriteDeadline(w, resolveStreamStallTimeout(gw))
	blockJSON, err := json.Marshal(blockPayload)
	if err != nil {
		gw.Logger.Error("failed to marshal blocked SSE payload", "err", err)
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", blockJSON)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// autoSaveTryServer runs MCP auto-save on one server. It returns true if the save
// succeeded; false means the caller may try another server.
func (p *Proxy) autoSaveTryServer(liveGW *gateway.NenyaGateway, agent *config.AgentConfig, serverName, agentName, assistantContent string, apiKey *config.ApiKey) bool {
	client, ok := liveGW.MCPClients[serverName]
	if !ok || !client.Ready() {
		return false
	}

	saveTool := agent.MCP.SaveTool
	if saveTool == "" {
		saveTool = p.discoverToolByPrefix(liveGW, serverName, "add")
		if saveTool == "" {
			saveTool = p.discoverToolByPrefix(liveGW, serverName, "save")
			if saveTool == "" {
				liveGW.Logger.Warn("MCP auto-save: no 'add'/'save' tool found on server",
					"server", serverName, "agent", agentName)
				return false
			}
		}
	}

	baseCtx := p.ShutdownCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	saveCtx, cancel := context.WithTimeout(baseCtx, mcpExecTimeout)
	defer cancel()

	saveArgs := map[string]any{
		"wing":     agentName,
		"room":     "conversation",
		"content":  assistantContent,
		"added_by": "nenya",
	}
	// The argument guard applies to auto-save too: the content is
	// model-controlled assistant text (size cap + URL policy).
	if guardErr := guardMCPArgs(liveGW, liveGW.Logger, mcpGuardDispatch{
		ServerName: serverName,
		ToolName:   saveTool,
		Purpose:    "auto_save",
		Args:       saveArgs,
		ApiKey:     apiKey,
	}); guardErr != nil {
		// Return false so the caller may fall back to another server
		// whose allowed_hosts permits the content; the guard already
		// logged the rejection.
		liveGW.Metrics.RecordMCPAutoSave(serverName, agentName, guardErr)
		return false
	}

	start := time.Now()
	_, err := client.CallTool(saveCtx, saveTool, saveArgs)
	duration := time.Since(start)
	if err != nil {
		liveGW.Logger.Warn("MCP auto-save failed (best-effort)",
			"server", serverName, "agent", agentName, "err", err,
			"duration_ms", duration.Milliseconds())
		liveGW.Metrics.RecordMCPAutoSave(serverName, agentName, err)
		return false
	}
	liveGW.Logger.Debug("MCP auto-save completed",
		"server", serverName, "agent", agentName,
		"duration_ms", duration.Milliseconds(),
		"content_len", len(assistantContent))
	liveGW.Metrics.RecordMCPAutoSave(serverName, agentName, nil)
	return true
}

func (p *Proxy) asyncMCPAutoSave(gw *gateway.NenyaGateway, agentName string, contentBuilder *contentBuilder, apiKey *config.ApiKey) {
	if agentName == "" || contentBuilder == nil {
		return
	}
	agent, ok := gw.Config.Agents[agentName]
	if !ok || agent.MCP == nil || !agent.MCP.AutoSave {
		return
	}

	assistantContent := contentBuilder.build()
	if assistantContent == "" {
		return
	}

	p.autoSaveWG.Add(1)
	go func() {
		defer p.autoSaveWG.Done()
		liveGW := p.Gateway()
		if liveGW == nil {
			return
		}
		agent, ok := liveGW.Config.Agents[agentName]
		if !ok || agent.MCP == nil || !agent.MCP.AutoSave {
			return
		}

		for _, serverName := range agent.MCP.Servers {
			if p.autoSaveTryServer(liveGW, &agent, serverName, agentName, assistantContent, apiKey) {
				return
			}
		}
	}()
}
