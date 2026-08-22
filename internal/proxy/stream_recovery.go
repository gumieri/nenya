package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
)

// streamResponseOpts groups the request-scoped context needed by streamResponse,
// including the routing context required to re-dispatch the same target when a
// stream continuation attempt is made.
type streamResponseOpts struct {
	gw           *gateway.NenyaGateway
	w            http.ResponseWriter
	r            *http.Request
	target       routing.UpstreamTarget
	agentName    string
	sourceFormat string
	cacheKey     string
	cooldown     time.Duration
	payload      map[string]any

	// Routing context used to re-dispatch the same target for continuation
	// attempts.
	targets    []routing.UpstreamTarget
	idx        int
	tokenCount int
	apiKey     *config.ApiKey
}

// streamContinuation carries the state for transparent stream continuation:
// when an upstream SSE stream is cut mid-generation (content seen, no
// finish_reason, no [DONE]), the proxy appends the partial assistant message
// and re-dispatches the same target so the client sees the stream keep flowing.
type streamContinuation struct {
	// capture accumulates the transformed output of the current attempt so a
	// partial assistant message can be rebuilt after a cut.
	capture *continuationCapture

	// maxAttempts is the total number of stream attempts including the
	// original dispatch (>= 1). A value of 2 means one continuation retry.
	maxAttempts int

	// includeReasoning controls whether partial reasoning_content is appended
	// to the continuation assistant message.
	includeReasoning bool
}

// newStreamContinuation returns a continuation state when transparent stream
// continuation is applicable, or nil when it must be disabled:
//   - governance.stream_continuation.enabled is false or unset;
//   - the effective max attempts is < 2 (no room for a continuation);
//   - the client uses the Anthropic Messages API or the target speaks the
//     Anthropic wire format (transformer-state carry-over is deferred to a
//     later phase).
func (p *Proxy) newStreamContinuation(gw *gateway.NenyaGateway, opts streamResponseOpts) *streamContinuation {
	if gw == nil || !gw.Config.Governance.StreamContinuationEnabled() {
		return nil
	}
	maxAttempts := gw.Config.Governance.EffectiveStreamContinuationMaxAttempts()
	if maxAttempts < 2 {
		return nil
	}
	if opts.sourceFormat == "anthropic" || opts.target.Format == "anthropic" {
		return nil
	}
	// Continuation always re-dispatches the exact same target; cross-model
	// fallback resume is not supported yet. An explicit same_model_only=false
	// therefore opts out rather than silently resuming onto a different model.
	if !gw.Config.Governance.StreamContinuationSameModelOnly() {
		return nil
	}
	return &streamContinuation{
		capture:          newContinuationCapture(gw.Config.Governance.EffectiveMaxTransformedSSEBytes()),
		maxAttempts:      maxAttempts,
		includeReasoning: gw.Config.Governance.StreamContinuationIncludeReasoning(),
	}
}

// finishStreamAttempt releases the resources of a single stream attempt before
// a continuation re-dispatch. Cancels the upstream context, closes the body,
// and drains the stall reader so its background goroutine does not leak.
func (p *Proxy) finishStreamAttempt(gw *gateway.NenyaGateway, action upstreamAction, stallR *stallReader) {
	action.cancel()
	_ = action.resp.Body.Close()
	if stallR != nil {
		drained, err := stallR.DrainPending(100 * time.Millisecond)
		if err != nil {
			gw.Logger.Debug("stall reader drain result", "drained_bytes", drained, "err", err)
		}
	}
}

// continuationCapture is a bounded tee sink that records the transformed bytes
// written to the client during a stream attempt. When the limit is exceeded the
// capture is abandoned (exceeded=true) so an unbounded partial assistant message
// is never built.
type continuationCapture struct {
	buf      bytes.Buffer
	maxBytes int
	exceeded bool
}

func newContinuationCapture(maxBytes int) *continuationCapture {
	if maxBytes <= 0 {
		maxBytes = 16 * 1024 * 1024
	}
	return &continuationCapture{maxBytes: maxBytes}
}

func (c *continuationCapture) Write(p []byte) (int, error) {
	if c.exceeded {
		return len(p), nil
	}
	if c.buf.Len()+len(p) > c.maxBytes {
		c.exceeded = true
		c.buf.Reset()
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *continuationCapture) reset() {
	c.buf.Reset()
	c.exceeded = false
}

// continuationTee wraps a destination writer and a continuationCapture, writing
// every byte to both. It is used to mirror the client-facing transformed output
// into the continuation accumulator.
type continuationTee struct {
	dst     io.Writer
	capture *continuationCapture
}

func (t *continuationTee) Write(p []byte) (int, error) {
	_, _ = t.capture.Write(p)
	return t.dst.Write(p)
}

// buildContinuationMessage parses the captured output of a cut attempt and
// returns the partial assistant message to append, or a give-up reason when the
// stream cannot be resumed. Reasons mirror the metric outcome labels:
// "gave_up_tool_call" when a tool call is in flight, "gave_up_no_content" when
// there is nothing meaningful to append.
func (p *Proxy) buildContinuationMessage(ctx context.Context, gw *gateway.NenyaGateway, cont *streamContinuation) (map[string]any, string) {
	if cont == nil || cont.capture == nil || cont.capture.exceeded {
		return nil, "gave_up_no_content"
	}

	buffered, err := bufferStreamResponse(ctx, bytes.NewReader(cont.capture.buf.Bytes()), gw.Logger)
	if err != nil {
		gw.Logger.Warn("stream continuation: failed to parse captured output",
			"err", err)
		return nil, "gave_up_no_content"
	}

	// A tool call without a finish_reason means arguments were cut mid-stream;
	// resuming would silently corrupt the tool call. Give up.
	if len(buffered.toolCalls) > 0 && buffered.finishReason == "" {
		return nil, "gave_up_tool_call"
	}

	if buffered.assistantMessage == nil || !buffered.hasContent {
		return nil, "gave_up_no_content"
	}

	msg := buffered.assistantMessage
	if !cont.includeReasoning {
		delete(msg, "reasoning_content")
	}
	return msg, ""
}

// appendAssistantMessage appends the partial assistant message to the payload's
// messages slice so the continuation attempt can resume from where it was cut.
func appendAssistantMessage(payload map[string]any, msg map[string]any) {
	msgs, ok := payload["messages"].([]any)
	if !ok {
		return
	}
	payload["messages"] = append(msgs, msg)
}

// writeStreamCutSSE terminates a stream that could not be continued with an
// OpenAI-compatible gateway_error event followed by [DONE]. Used when the cut
// error injection was suppressed because a continuation attempt was possible
// but ultimately given up.
func writeStreamCutSSE(w http.ResponseWriter) {
	writeSSEError(w, http.StatusOK, "upstream stream ended without [DONE]")
}

// streamContinueStatus describes the result of a continuation attempt.
type streamContinueStatus int

const (
	// streamContinueOK means the continuation re-dispatch succeeded and the
	// caller must stream the new action.
	streamContinueOK streamContinueStatus = iota
	// streamContinueGaveUp means the continuation was not possible and the
	// gateway_error terminator has already been written to the client.
	streamContinueGaveUp
)

// isStreamCut reports whether the upstream stream was cut mid-generation:
// content was seen but no finish_reason and no [DONE] marker arrived.
func isStreamCut(cont *streamContinuation, transformingReader *stream.SSETransformingReader, copyErr error) bool {
	if cont == nil || transformingReader == nil || copyErr != nil {
		return false
	}
	return transformingReader.SawContent() &&
		!transformingReader.SawFinishReason() &&
		!transformingReader.SawDone()
}

// tryContinueStream attempts to resume a cut stream by rebuilding the partial
// assistant message from the captured output and re-dispatching the same
// target. On success the new action is returned with streamContinueOK; on any
// give-up path the gateway_error terminator is written to the client and
// streamContinueGaveUp is returned.
func (p *Proxy) tryContinueStream(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, opts streamResponseOpts, target routing.UpstreamTarget, payload map[string]any, cooldownDuration time.Duration, action upstreamAction, cont *streamContinuation, stallR *stallReader, buf *[]byte) (streamContinueStatus, upstreamAction) {
	msg, giveUpReason := p.buildContinuationMessage(r.Context(), gw, cont)
	if giveUpReason != "" {
		gw.Metrics.RecordStreamContinuation(target.Model, target.Provider, giveUpReason)
		p.finishStreamAttempt(gw, action, stallR)
		writeStreamCutSSE(w)
		putStreamBuffer(buf)
		return streamContinueGaveUp, action
	}

	appendAssistantMessage(payload, msg)
	gw.Metrics.RecordStreamContinuation(target.Model, target.Provider, "recovered")

	p.finishStreamAttempt(gw, action, stallR)

	newAction := p.prepareAndSend(gw, r, opts.idx, opts.targets, target, payload, cooldownDuration, opts.tokenCount, opts.agentName, opts.apiKey, true)
	if newAction.kind != actionStream {
		gw.Metrics.RecordStreamContinuation(target.Model, target.Provider, "gave_up_redispatch")
		if newAction.cancel != nil {
			newAction.cancel()
		}
		if newAction.resp != nil {
			_ = newAction.resp.Body.Close()
		}
		writeStreamCutSSE(w)
		putStreamBuffer(buf)
		return streamContinueGaveUp, action
	}
	return streamContinueOK, newAction
}
