package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/billing"
	"github.com/nenya/internal/gateway"
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

func getStreamBuffer() *[]byte {
	buf := streamingBufPool.Get().(*[]byte)
	clear(*buf)
	return buf
}

// putStreamBuffer returns a buffer to the pool after restoring its full length.
// The buffer pointer is modified to point to the full-capacity slice before pooling.
// Buffers with incorrect capacity are dropped with a Warn log to prevent pool pollution.
func putStreamBuffer(buf *[]byte) {
	if buf == nil || *buf == nil {
		return
	}
	if cap(*buf) != streamBufferSize {
		slog.Warn("dropping mis-sized buffer from pool", "expected_cap", streamBufferSize, "actual_cap", cap(*buf))
		return
	}
	*buf = (*buf)[:cap(*buf)]
	streamingBufPool.Put(buf)
}

// contentBuilder accumulates SSE content chunks for MCP auto-save.
type contentBuilder struct {
	buf strings.Builder
}

func newContentBuilder() *contentBuilder {
	return &contentBuilder{}
}

func (b *contentBuilder) addContent(s string) {
	b.buf.WriteString(s)
}

func (b *contentBuilder) build() string {
	return b.buf.String()
}

type readResult struct {
	data       []byte
	err        error
	poolBufPtr *[]byte // pool buffer to return after data is fully consumed
}

// stallReader wraps an io.Reader and detects stalls where no data is received
// within the configured timeout. It runs a background goroutine to read from
// the underlying source and signals stall detection via a channel. The stallReader
// supports thinking-aware timeout extension: when SetThinkingActive(true) is called
// (by the SSETransformingReader detecting reasoning content), the stall timeout
// automatically extends to thinkingTimeout instead of the base timeout.
type stallReader struct {
	mu              sync.Mutex
	timer           *time.Timer
	timeout         time.Duration
	thinkingTimeout time.Duration
	thinkingActive  atomic.Bool
	stalled         bool
	stallCh         chan struct{}
	closeOnce       sync.Once
	srcOnce         sync.Once
	srcCloser       io.Closer
	srcErr          error
	ch              chan readResult
	remainBuf       []byte
	remainPos       int
	// pendingErr retains a terminal read error (EOF or transport failure) seen
	// by the background reader so it is returned only after buffered bytes have
	// been drained, never deadlocking or dropping the stream tail.
	pendingErr error
}

// activeTimeout returns the appropriate timeout based on the current thinking state.
// Uses atomic read, safe to call without holding sr.mu.
func (sr *stallReader) activeTimeout() time.Duration {
	if sr.thinkingActive.Load() {
		return sr.thinkingTimeout
	}
	return sr.timeout
}

// SetThinkingActive atomically transitions the thinking state and resets the
// stall timer with the appropriate timeout. This method is thread-safe and can be
// called concurrently with Read(). If the state changes, the timer is reset to
// extend or contract the stall window based on the new state. When
// thinkingTimeout is 0 (disabled), this method is a no-op to avoid Reset(0)
// which fires immediately.
func (sr *stallReader) SetThinkingActive(active bool) {
	if sr.thinkingTimeout <= 0 {
		return
	}
	sr.mu.Lock()
	was := sr.thinkingActive.Swap(active)
	if was != active && !sr.stalled {
		sr.timer.Reset(sr.activeTimeout())
	}
	sr.mu.Unlock()
}

// newStallReader creates a stallReader that reads from src with the given
// base timeout and extended timeout for thinking phases. The context is used
// to cancel the background read goroutine on shutdown.
func newStallReader(ctx context.Context, src io.Reader, timeout, thinkingTimeout time.Duration) *stallReader {
	sr := &stallReader{
		timeout:         timeout,
		thinkingTimeout: thinkingTimeout,
		stallCh:         make(chan struct{}),
		ch:              make(chan readResult, 1),
	}
	sr.timer = time.AfterFunc(sr.activeTimeout(), func() {
		sr.mu.Lock()
		sr.stalled = true
		sr.mu.Unlock()
		sr.closeOnce.Do(func() { close(sr.stallCh) })
	})
	if closer, ok := src.(io.Closer); ok {
		sr.srcCloser = closer
	}
	go sr.readLoop(ctx, src)
	return sr
}

// Close releases the upstream stream source. Idempotent and safe to call
// concurrently with Read; the background read goroutine observes the closed
// source and terminates on its next read. The stall signal is also fired so a
// Read blocked waiting on data wakes promptly. The srcCloser.Close() error is
// returned (first call wins).
func (sr *stallReader) Close() error {
	sr.srcOnce.Do(func() {
		if sr.srcCloser != nil {
			sr.srcErr = sr.srcCloser.Close()
		}
	})
	sr.mu.Lock()
	if !sr.stalled {
		sr.stalled = true
	}
	sr.mu.Unlock()
	sr.closeOnce.Do(func() { close(sr.stallCh) })
	return sr.srcErr
}

// DrainBuffered releases pooled resources when a stream is abandoned
// (failover/abort) so neither a queued read-ahead result nor an unread tail is
// held until the request context cancels. Also drops any buffered remainder so
// its backing slice is eligible for collection.
//
// The abort paths close the upstream source before calling this, so the
// background reader's in-flight read is unblocked and its terminal result is
// imminent; a short bounded receive after the non-blocking sweep catches it and
// returns its pooled buffer instead of stranding it.
func (sr *stallReader) DrainBuffered() {
	sr.mu.Lock()
	sr.remainBuf = nil
	sr.remainPos = 0
	sr.mu.Unlock()
	release := func(rr readResult) {
		if rr.poolBufPtr != nil {
			putStreamBuffer(rr.poolBufPtr)
		}
	}
	for {
		select {
		case rr := <-sr.ch:
			release(rr)
		default:
			goto swept
		}
	}
swept:
	t := time.NewTimer(50 * time.Millisecond)
	defer t.Stop()
	select {
	case rr := <-sr.ch:
		release(rr)
	case <-t.C:
	}
}

func (sr *stallReader) readLoop(ctx context.Context, src io.Reader) {
	var poolBufPtr *[]byte
	for {
		if poolBufPtr == nil {
			poolBufPtr = getStreamBuffer()
		}
		n, err := src.Read((*poolBufPtr)[:])
		var data []byte
		if n > 0 {
			*poolBufPtr = (*poolBufPtr)[:n]
			data = *poolBufPtr
		}
		select {
		case sr.ch <- readResult{data: data, err: err, poolBufPtr: poolBufPtr}:
		case <-ctx.Done():
			if poolBufPtr != nil {
				putStreamBuffer(poolBufPtr)
			}
			return
		}
		if err != nil {
			return
		}
		poolBufPtr = nil
	}
}

// Read reads data from the upstream source, buffering any excess bytes from a
// single readResult that do not fit in the caller's buffer. Buffered bytes are
// returned on subsequent Read calls before reading from the channel again.
//
// A terminal error delivered by the background reader (e.g. io.EOF) is retained
// and returned only after every buffered byte has been served, so a caller that
// consumes data and its terminating error from the same upstream read never
// loses the tail or deadlocks waiting for a second channel message.
func (sr *stallReader) Read(p []byte) (int, error) {
	sr.mu.Lock()
	if sr.stalled {
		sr.mu.Unlock()
		return 0, errStreamStalled
	}

	if sr.remainPos < len(sr.remainBuf) {
		n := copy(p, sr.remainBuf[sr.remainPos:])
		sr.remainPos += n
		if sr.remainPos >= len(sr.remainBuf) {
			sr.remainBuf = nil
			sr.remainPos = 0
		}
		// Serving a buffered tail is still stream activity; keep the idle
		// deadline coherent so a slow consumer trickling out a large remainder
		// is not spuriously classified as a stalled upstream.
		if !sr.stalled {
			sr.timer.Reset(sr.activeTimeout())
		}
		sr.mu.Unlock()
		return n, nil
	}

	if sr.pendingErr != nil {
		err := sr.pendingErr
		sr.mu.Unlock()
		return 0, err
	}
	sr.mu.Unlock()

	select {
	case <-sr.stallCh:
		return 0, errStreamStalled
	case rr := <-sr.ch:
		return sr.serveResult(p, rr)
	}
}

// serveResult relays a readResult produced by the background reader, retaining
// a terminal error in pendingErr and re-buffering any head bytes that do not
// fit the caller's buffer. Shared tail state is updated under sr.mu so a
// concurrent DrainBuffered/DrainPending never observes a half-updated
// remainBuf.
func (sr *stallReader) serveResult(p []byte, rr readResult) (int, error) {
	if rr.poolBufPtr != nil {
		defer putStreamBuffer(rr.poolBufPtr)
	}
	sr.mu.Lock()
	stalled := sr.stalled
	if rr.err != nil {
		sr.pendingErr = rr.err
	}
	if !stalled && len(rr.data) > 0 {
		sr.timer.Reset(sr.activeTimeout())
	}
	if stalled {
		sr.mu.Unlock()
		return 0, errStreamStalled
	}
	n := copy(p, rr.data)
	remaining := len(rr.data) - n
	if remaining < 0 {
		remaining = 0
	}
	buffered := remaining > 0
	if buffered {
		sr.remainBuf = make([]byte, remaining)
		copy(sr.remainBuf, rr.data[n:])
		sr.remainPos = 0
	}
	sr.mu.Unlock()
	if buffered {
		// Tail buffered: serve it before reporting the terminal error.
		return n, nil
	}
	// Whole result fits the caller's buffer: report data and its terminal
	// error together (standard Read contract), with pendingErr as a backup
	// for callers that already consumed the stream head.
	return n, rr.err
}

// Stop stops the stall reader timer and marks the reader as stalled.
// Safe to call multiple times. The closeOnce.Do pattern ensures the channel
// is closed exactly once even when Stop races with the timer callback.
func (sr *stallReader) Stop() {
	sr.mu.Lock()
	sr.timer.Stop()
	if !sr.stalled {
		sr.stalled = true
		sr.mu.Unlock()
		sr.closeOnce.Do(func() { close(sr.stallCh) })
	} else {
		sr.mu.Unlock()
	}
}

// DrainPending reads any remaining buffered data from the reader with the given timeout.
// Returns the number of bytes drained (including remainBuf and drained channel messages) and any error.
func (sr *stallReader) DrainPending(timeout time.Duration) (int, error) {
	sr.closeOnce.Do(func() { close(sr.stallCh) })

	total := 0
	sr.mu.Lock()
	if sr.remainPos < len(sr.remainBuf) {
		total += len(sr.remainBuf) - sr.remainPos
		sr.remainBuf = nil
		sr.remainPos = 0
	}
	sr.mu.Unlock()

	// Block on first message (or timeout)
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case rr := <-sr.ch:
		if rr.poolBufPtr != nil {
			putStreamBuffer(rr.poolBufPtr)
		}
		total += len(rr.data)
		// Drain remaining messages without blocking to prevent pool buffer leaks
		for {
			select {
			case pending := <-sr.ch:
				if pending.poolBufPtr != nil {
					putStreamBuffer(pending.poolBufPtr)
				}
				total += len(pending.data)
			default:
				// No more pending messages
				return total, rr.err
			}
		}
	case <-t.C:
		return total, fmt.Errorf("stall reader drain timeout: drained %d bytes", total)
	}
}

var errStreamStalled = errors.New("stream stalled: no data received within idle timeout")

var (
	errClientWriteSide  = errors.New("client write")
	errUpstreamReadSide = errors.New("upstream read")
)

func isClientWriteError(err error) bool {
	return errors.Is(err, errClientWriteSide)
}

// streamResult carries the outcome of streamResponse back to the retry loop.
// empty signals a zero-byte upstream stream; err signals a transport-level read
// failure before any stream bytes were produced (distinct from empty).
type streamResult struct {
	empty bool
	err   error
}

// prefixedReadCloser wraps an io.Reader with a prefix buffer that is
// returned before delegating to the underlying reader. Used to prepend
// already-read bytes to a stream body.
type prefixedReadCloser struct {
	prefix []byte
	pos    int
	reader io.ReadCloser
}

func (p *prefixedReadCloser) Read(buf []byte) (int, error) {
	if p.pos < len(p.prefix) {
		n := copy(buf, p.prefix[p.pos:])
		p.pos += n
		return n, nil
	}
	return p.reader.Read(buf)
}

func (p *prefixedReadCloser) Close() error {
	return p.reader.Close()
}

// immediateFlushWriter wraps an http.ResponseWriter and flushes after every
// Write call when the underlying writer supports http.Flusher.
type immediateFlushWriter struct {
	dst     http.ResponseWriter
	flusher http.Flusher
}

// newImmediateFlushWriter creates an immediateFlushWriter if the response writer
// supports http.Flusher. The boolean indicates whether Flusher was available.
func newImmediateFlushWriter(w http.ResponseWriter) (*immediateFlushWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return &immediateFlushWriter{dst: w}, false
	}
	return &immediateFlushWriter{dst: w, flusher: flusher}, true
}

func (fw *immediateFlushWriter) Write(p []byte) (int, error) {
	n, err := fw.dst.Write(p)
	if err == nil && fw.flusher != nil {
		fw.flusher.Flush()
	}
	return n, err
}

func (fw *immediateFlushWriter) Header() http.Header {
	return fw.dst.Header()
}

func (fw *immediateFlushWriter) WriteHeader(statusCode int) {
	fw.dst.WriteHeader(statusCode)
}

// sseTeeWriter copies SSE output to a buffer while forwarding to the client,
// up to a configurable maximum to bound memory usage.
type sseTeeWriter struct {
	dst      io.Writer
	buf      *bytes.Buffer
	maxBytes int64
	exceeded bool
}

func (t *sseTeeWriter) Write(p []byte) (int, error) {
	if !t.exceeded {
		if t.maxBytes > 0 && int64(t.buf.Len()+len(p)) > t.maxBytes {
			t.exceeded = true
		} else {
			t.buf.Write(p)
		}
	}
	return t.dst.Write(p)
}

// copyStream copies data from src to dst using the provided buffer, respecting context cancellation.
// Returns the number of bytes copied and any error encountered.
func copyStream(ctx context.Context, dst io.Writer, src io.Reader, buf []byte) (int64, error) {
	if len(buf) == 0 {
		buf = make([]byte, streamBufferSize)
	}
	var written int64
	for {
		nr, rerr := src.Read(buf)
		if nr > 0 {
			var werr error
			written, werr = writeStreamChunk(ctx, dst, buf[:nr], written)
			if werr != nil {
				return written, werr
			}
		}
		if rerr != nil {
			return written, classifyStreamReadErr(rerr)
		}
		if ctx.Err() != nil {
			return written, ctx.Err()
		}
	}
}

// writeStreamChunk writes a single read chunk to dst, folding client-side write
// failures into the client-side error sentinel and tracking progress.
func writeStreamChunk(ctx context.Context, dst io.Writer, chunk []byte, written int64) (int64, error) {
	nw, werr := dst.Write(chunk)
	if werr != nil {
		if ctx.Err() != nil {
			return written, ctx.Err()
		}
		return written, fmt.Errorf("%w: %v", errClientWriteSide, werr)
	}
	if len(chunk) != nw {
		return written, io.ErrShortWrite
	}
	return written + int64(nw), nil
}

// classifyStreamReadErr maps an upstream stream read error to its sentinel,
// translating io.EOF into a clean (nil) termination and preserving context
// cancellation as-is.
func classifyStreamReadErr(rerr error) error {
	if rerr == io.EOF {
		return nil
	}
	if errors.Is(rerr, context.Canceled) || errors.Is(rerr, context.DeadlineExceeded) {
		return rerr
	}
	return fmt.Errorf("%w: %v", errUpstreamReadSide, rerr)
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
		return p.failoverEarlyHeadError(gw, action, target, cooldownDuration, stallR)
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

// failoverEarlyHeadError fails over before any client byte is committed: the
// upstream body is closed and the retry loop dispatches the next target via
// streamResult{empty}.
func (p *Proxy) failoverEarlyHeadError(gw *gateway.NenyaGateway, action upstreamAction, target routing.UpstreamTarget, cooldownDuration time.Duration, stallR *stallReader) *streamResult {
	if err := action.resp.Body.Close(); err != nil {
		gw.Logger.Debug("close errored upstream body", "err", err, "provider", target.Provider)
	}
	if stallR != nil {
		stallR.Stop()
		stallR.DrainBuffered()
	}
	action.cancel()
	gw.AgentState.RecordFailure(target, cooldownDuration)
	gw.Metrics.RecordEarlyStreamError(target.Model, target.Provider, "failover")
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
// run when at least one of the governing config flags is enabled.
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

	gw.ExtractQuotaFromResponseHeaders(r.Context(), target.Provider, target.AccountName, action.resp.Header)

	buf := getStreamBuffer()
	flushWriter, canFlush := newImmediateFlushWriter(w)
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
		transformingReader, contentBuilder, stallR := p.setupTransformingReader(gw, target, opts.agentName, opts.sourceFormat, action, r.Context(), attemptStall)
		if transformingReader == nil {
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
				action = newAction
				// The action swap happened for this attempt; the new action's
				// http response carries the continuation's StatusCode, which is
				// equal to the original (both should be 200). Ensure the
				// deferred cancel on streamResponse doesn't leak the previous
				// action's context.
				defer action.cancel()
				continue
			}
			return streamResult{}
		}

		// Final attempt: on a suppressed (non-continuable) cut the reader
		// already injected its terminator, so finalize without polluting the
		// response cache with the partial cut stream.
		if isStreamCut(cont, transformingReader, copyErr) {
			gw.Metrics.RecordStreamContinuation(target.Model, target.Provider, "gave_up_exhausted")
			return p.handleStreamDone(gw, w, target, opts.agentName, action, "", cooldownDuration, payload, buf, copyErr, nil, nil, contentBuilder, stallR, r.Context())
		}

		return p.handleStreamDone(gw, w, target, opts.agentName, action, cacheKey, cooldownDuration, payload, buf, copyErr, captureBuf, tee, contentBuilder, stallR, r.Context())
	}

	putStreamBuffer(buf)
	return streamResult{}
}

// resolveTransformer selects the appropriate SSE transformer based on source format and target format.
func (p *Proxy) resolveTransformer(gw *gateway.NenyaGateway, target routing.UpstreamTarget, sourceFormat string) stream.ResponseTransformer {
	if sourceFormat == "anthropic" && target.Format != "anthropic" {
		gw.Logger.Debug("SSE reverse transformer active (Anthropic client, OpenAI upstream)", "provider", target.Provider)
		return stream.NewOpenAIToAnthropicTransformer()
	}

	if target.Format == "anthropic" {
		gw.Logger.Debug("SSE transformer active", "provider", target.Provider, "format", target.Format)
		return stream.NewAnthropicTransformer()
	}

	if spec, ok := providerpkg.Get(target.Provider); ok && spec.NewResponseTransformer != nil {
		transformer := spec.NewResponseTransformer(gw.ThoughtSigCache)
		if transformer != nil {
			gw.Logger.Debug("SSE transformer active", "provider", target.Provider)
		}
		return transformer
	}

	return nil
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

// setupTransformingReader creates and configures the SSE transforming reader, content builder, and stall reader.
// Returns the transforming reader for streaming, the content builder for post-processing, and the stall reader for cleanup.
// When stream idle timeout is 0 (disabled), stallR is nil and the body is passed directly to the transforming reader.
// reuse, when non-nil, is a stall reader already built around this body (by the
// stream-head probe); it is used as-is so its buffered read-ahead bytes stay in
// line, and backgroundExhaust is not double-allocated.
func (p *Proxy) setupTransformingReader(gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName, sourceFormat string, action upstreamAction, ctx context.Context, reuse *stallReader) (*stream.SSETransformingReader, *contentBuilder, *stallReader) {
	transformer := p.resolveTransformer(gw, target, sourceFormat)

	var bodyReader io.Reader = action.resp.Body
	var stallR = reuse
	if stallR == nil {
		timeout := resolveStreamIdleTimeout(gw, target.Provider)
		if timeout > 0 {
			thinkingTimeout := gw.Config.Governance.EffectiveThinkingStreamIdleTimeout()
			stallR = newStallReader(ctx, action.resp.Body, timeout, thinkingTimeout)
			bodyReader = stallR
		}
	} else if _, wrapped := action.resp.Body.(*prefixedReadCloser); wrapped {
		// The stream-head probe wrapped the body (prefix over the reused stall
		// reader): keep reading through the wrapper so byte order is preserved.
		bodyReader = action.resp.Body
	} else {
		// Probe disabled: the reused stall reader's goroutine is the sole
		// consumer of the raw body. Reading the raw body directly here would
		// race it and starve the stream, so read through the stall reader.
		bodyReader = stallR
	}

	transformingReader := stream.NewSSETransformingReader(bodyReader, transformer, ctx)
	transformingReader.SetMaxTransformedBytes(gw.Config.Governance.EffectiveMaxTransformedSSEBytes())
	transformingReader.SetOnUsage(p.makeUsageCallback(ctx, gw, target, agentName))
	transformingReader.SetObserver(newUpstreamErrorObserver(gw, target))
	transformingReader.SetLogger(gw.Logger)

	p.setupStreamFilterIfEnabled(gw, transformingReader)
	p.setupStreamEntropyFilterIfEnabled(gw, transformingReader)
	contentBuilder := p.setupContentBuilderIfNeeded(gw, agentName, transformingReader)

	if stallR != nil {
		transformingReader.SetOnThinking(func(active bool) {
			stallR.SetThinkingActive(active)
		})
	}

	return transformingReader, contentBuilder, stallR
}

// upstreamErrorObserver is an SSE observer that detects error events within
// a 200 OK stream and records them as circuit breaker failures.
type upstreamErrorObserver struct {
	gw     *gateway.NenyaGateway
	target routing.UpstreamTarget
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
	if event.GatewayInjected {
		// Errors synthesized by the gateway (e.g. a gateway_error injected
		// when an upstream stream ends without [DONE]) are not provider
		// failures and must not count toward the circuit breaker.
		o.gw.Logger.Debug("ignoring gateway-injected error event for circuit breaker",
			"model", o.target.Model, "provider", o.target.Provider)
		return
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

// makeUsageCallback returns a callback function that records token usage statistics.
// The callback is invoked by the SSE transformer when usage metadata is received.
// The ctx parameter is reserved for future timeout/cancellation logic in cost tracking.
func (p *Proxy) makeUsageCallback(ctx context.Context, gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string) func(stream.UsageData) {
	return func(u stream.UsageData) {
		completion, prompt := u.CompletionTokens, u.PromptTokens
		cacheHit, cacheMiss := u.CacheHitTokens, u.CacheMissTokens
		cacheCreation, reasoning := u.CacheCreationTokens, u.ReasoningTokens
		if completion > 0 {
			gw.Stats.RecordOutput(target.Model, completion)
			gw.Metrics.RecordTokens("output", target.Model, agentName, target.Provider, completion)
		}
		if reasoning > 0 {
			gw.Stats.RecordReasoning(target.Model, reasoning)
			gw.Metrics.RecordTokens(TokenDirectionReasoning, target.Model, agentName, target.Provider, reasoning)
		}
		if cacheHit > 0 {
			gw.Stats.RecordCacheHit(target.Model, cacheHit)
			gw.Metrics.RecordCacheReadTokens(target.Model, agentName, target.Provider, cacheHit)
		}
		if cacheMiss > 0 {
			gw.Stats.RecordCacheMiss(target.Model, cacheMiss)
			gw.Metrics.RecordCacheMissTokens(target.Model, agentName, target.Provider, cacheMiss)
		}
		if cacheCreation > 0 {
			gw.Stats.RecordCacheCreation(target.Model, cacheCreation)
			gw.Metrics.RecordCacheCreationTokens(target.Model, agentName, target.Provider, cacheCreation)
		}
		// Note: Stats cache methods don't track agent/provider; Metrics methods do.
		// This pattern (if count > 0 { Stats.*; Metrics.* }) is intentional.
		if gw.CostTracker != nil && (prompt > 0 || completion > 0) {
			if dm, ok := gw.ModelCatalog.Lookup(target.Model); ok && dm.Pricing != nil && !dm.Pricing.IsZero() {
				cost := dm.Pricing.CalculateCost(int64(prompt), int64(completion))
				gw.CostTracker.RecordUsage(target.Model, cost)
				if gw.BillingTracker != nil {
					gw.BillingTracker.RecordSpend(ctx, billing.SpendEntry{
						ProviderName: target.Provider,
						AccountName:  target.AccountName,
						RequestID:    "",
						InputTokens:  prompt,
						OutputTokens: completion,
						CostUSD:      cost,
						Timestamp:    time.Now(),
					})
				}
			}
		}
	}
}

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

func (p *Proxy) handleStreamDone(gw *gateway.NenyaGateway, w http.ResponseWriter, target routing.UpstreamTarget, agentName string, action upstreamAction, cacheKey string, cooldownDuration time.Duration, payload map[string]any, buf *[]byte, copyErr error, captureBuf *bytes.Buffer, tee *sseTeeWriter, contentBuilder *contentBuilder, stallR *stallReader, reqCtx context.Context) streamResult {
	putStreamBuffer(buf)

	if errors.Is(copyErr, stream.ErrStreamBlocked) {
		action.cancel()
		_ = action.resp.Body.Close()
		gw.Logger.Warn("stream blocked by execution policy, upstream killed",
			"model", target.Model, "provider", target.Provider)
		gw.Metrics.RecordStreamBlock(target.Model, target.Provider)
		p.writeBlockedSSE(gw, w)
		return streamResult{}
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
		gw.Logger.Warn("stream stalled, terminating SSE before fallback",
			"agent", agentName, "model", target.Model, "provider", target.Provider,
			"idle_timeout", gw.Config.Governance.EffectiveStreamIdleTimeout())
		gw.Metrics.RecordStreamStall(target.Model, target.Provider)
		p.writeStallSSE(gw, w)
		return streamResult{empty: true}
	}

	if isClientWriteError(copyErr) {
		gw.Logger.Info("client disconnected mid-write, aborting upstream stream",
			"model", target.Model, "provider", target.Provider)
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

	recordStreamResult(gw, target, agentName, cooldownDuration, copyErr)

	if copyErr == nil {
		storeStreamCache(gw, cacheKey, captureBuf, tee, payload, reqCtx)
		p.asyncMCPAutoSave(gw, agentName, contentBuilder)
		return streamResult{}
	}
	if errors.Is(copyErr, context.Canceled) || errors.Is(copyErr, context.DeadlineExceeded) {
		return streamResult{}
	}
	writeSSEError(w, http.StatusOK, "upstream stream interrupted")
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

// recordStreamResult records the outcome of a streaming request to the agent state and metrics.
func recordStreamResult(gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string, cooldownDuration time.Duration, copyErr error) {
	if copyErr == nil {
		gw.AgentState.RecordSuccess(target.CoolKey)
		return
	}
	if errors.Is(copyErr, context.Canceled) || errors.Is(copyErr, context.DeadlineExceeded) {
		gw.Logger.Debug("stream ended", "model", target.Model, "provider", target.Provider, "reason", streamErrorReason(copyErr))
		gw.AgentState.RecordSuccess(target.CoolKey)
		return
	}
	gw.Logger.Warn("stream copy error (upstream)",
		"model", target.Model, "provider", target.Provider, "err", copyErr)
	gw.AgentState.RecordFailure(target, cooldownDuration)
}

func storeStreamCache(gw *gateway.NenyaGateway, cacheKey string, captureBuf *bytes.Buffer, tee *sseTeeWriter, payload map[string]any, reqCtx context.Context) {
	if cacheKey == "" || gw.ResponseCache == nil || tee == nil || tee.exceeded || captureBuf.Len() <= 0 {
		return
	}

	captured := captureBuf.Bytes()

	// Skip caching for refusal responses. The check is a simple substring match for performance.
	// False positives are acceptable (conservative cache miss) and extremely unlikely in practice:
	// - "refusal" and "content_filter" are technical field values (finish_reason, stop_reason), not conversational content
	// - Models don't refuse via text content; they set structured stop_reason fields
	// - Worst case: cache miss for non-refusal response (conservative)
	if bytes.Contains(captured, []byte("refusal")) || bytes.Contains(captured, []byte("content_filter")) {
		return
	}

	var embedding []float32
	if gw.Config.ResponseCache.EnableSemantic && payload != nil {
		embedding = computeEmbedding(gw, payload, reqCtx)
	}

	gw.ResponseCache.Store(cacheKey, captured, embedding)
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
	errPayload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": "upstream stream stalled: no data received within idle timeout",
			"type":    "gateway_error",
		},
	})
	if err != nil {
		gw.Logger.Error("failed to marshal stall error payload", "err", err)
		errPayload = []byte(`{"error":{"message":"upstream stream stalled","type":"gateway_error"}}`)
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", errPayload)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (p *Proxy) writeTimeoutSSE(gw *gateway.NenyaGateway, w http.ResponseWriter) {
	errPayload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": "upstream timeout reached",
			"type":    "gateway_error",
		},
	})
	if err != nil {
		gw.Logger.Error("failed to marshal timeout error payload", "err", err)
		errPayload = []byte(`{"error":{"message":"upstream timeout reached","type":"gateway_error"}}`)
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", errPayload)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// writeBlockedSSE sends a blocked response SSE stream to the client.
// This is used when the execution policy blocks a request.
func (p *Proxy) writeBlockedSSE(gw *gateway.NenyaGateway, w http.ResponseWriter) {
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
func (p *Proxy) autoSaveTryServer(liveGW *gateway.NenyaGateway, agent *config.AgentConfig, serverName, agentName, assistantContent string) bool {
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

	start := time.Now()
	_, err := client.CallTool(saveCtx, saveTool, map[string]any{
		"wing":     agentName,
		"room":     "conversation",
		"content":  assistantContent,
		"added_by": "nenya",
	})
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

func (p *Proxy) asyncMCPAutoSave(gw *gateway.NenyaGateway, agentName string, contentBuilder *contentBuilder) {
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

	go func() {
		liveGW := p.Gateway()
		if liveGW == nil {
			return
		}
		agent, ok := liveGW.Config.Agents[agentName]
		if !ok || agent.MCP == nil || !agent.MCP.AutoSave {
			return
		}

		for _, serverName := range agent.MCP.Servers {
			if p.autoSaveTryServer(liveGW, &agent, serverName, agentName, assistantContent) {
				return
			}
		}
	}()
}
