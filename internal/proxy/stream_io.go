package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

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
	// ctx is the request-scoped context the reader was built with. Read
	// watches its Done so a blocked consumer wakes immediately on client
	// disconnect instead of parking until the stall deadline.
	ctx       context.Context
	remainBuf []byte
	remainPos int
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
		ctx:             ctx,
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
	case <-sr.ctx.Done():
		// Request teardown (client disconnect, server shutdown) must
		// interrupt a blocked read immediately: leaving it parked lets
		// upstream inference (and billing) run on for a dead client.
		return 0, sr.ctx.Err()
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
	// terminal marks a response already fully written to the client
	// (e.g. a content-policy block): the retry loop must stop instead of
	// failing over and appending another response to the committed one.
	terminal bool
	err      error
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
