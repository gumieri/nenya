package stream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"reflect"
	"strconv"
	"sync"

	"github.com/nenya/internal/util"
)

// ErrEventConsumed signals that the transformer consumed the SSE event
// and no output should be forwarded to the client.
var ErrEventConsumed = errors.New("sse event consumed by transformer")

// ErrTransformedSizeExceeded is returned when the accumulated transformed
// SSE output exceeds the maximum allowed size.
var ErrTransformedSizeExceeded = errors.New("transformed SSE output size exceeded")

const (
	SSEScannerInitialBuf = 64 * 1024
	SSEScannerMaxBuf     = 1024 * 1024
	// maxTransformedBytes caps the accumulated transformed output to
	// prevent memory exhaustion from malicious or buggy upstreams that send
	// many small events producing unbounded transformed output.
	DefaultMaxTransformedEventBytes = 50 * 1024 * 1024
)

type ResponseTransformer interface {
	TransformSSEChunk(ctx context.Context, data []byte) ([]byte, error)
}

// UsageCallback is invoked by the SSE transformer when usage metadata is received.
// UsageCallback receives token usage statistics from the stream.
// Parameters are deltas since the last callback (each field >= 0).
type UsageCallback func(UsageData)

type UsageData struct {
	CompletionTokens         int
	PromptTokens             int
	TotalTokens              int
	CacheHitTokens           int
	CacheMissTokens          int
	CacheCreationTokens      int
	ReasoningTokens          int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
}

type ContentCallback func(content string)

type ThinkingCallback func(active bool)

// SSEObserver receives notifications about SSE events during streaming.
// Observers are called after transformation, so they see what the client receives;
// the Raw line is the PRE-filter upstream bytes when a filter rewrote the chunk
// (Data carries the post-filter text).
type SSEObserver interface {
	// OnSSEEvent is called for each SSE event (data line, [DONE], error, etc.)
	OnSSEEvent(event SSEEvent)
	// OnStreamClose is called when the stream ends (with any error, or nil on clean EOF)
	OnStreamClose(err error)
}

// SSEEvent represents a single SSE event.
type SSEEvent struct {
	ID   string
	Type string // "content", "usage", "tool_call", "done", "error"
	Data map[string]interface{}
	Raw  []byte
	// GatewayInjected marks events synthesized by the gateway itself (e.g. a
	// gateway_error injected when an upstream stream ends without [DONE]).
	// Observers use this to distinguish gateway failures from real provider
	// errors, and should not attribute them to the provider.
	GatewayInjected bool
}

// SSETransformingReader reads from an SSE source and transforms SSE data lines.
type SSETransformingReader struct {
	src                 io.Reader
	scanner             *bufio.Scanner
	transformer         ResponseTransformer
	onUsage             UsageCallback
	onContent           ContentCallback
	onThinking          ThinkingCallback
	observer            SSEObserver
	streamFilter        *StreamFilter
	exfilGuard          *ExfilGuard
	streamEntropyFilter *StreamEntropyFilter
	buffer              []byte
	pos                 int
	err                 error
	closed              bool
	sawDone             bool
	sawContent          bool
	sawFinishReason     bool
	injectedError       bool
	ctx                 context.Context
	poolBuf             *[]byte
	logger              *slog.Logger

	// last* fields track token usage between consecutive usage chunks.
	// They are accessed only from the single goroutine calling Read().
	// No mutex needed; callers must not call Read() concurrently.
	lastCompletionTokens    int
	lastPromptTokens        int
	lastTotalTokens         int
	lastCacheHitTokens      int
	lastCacheMissTokens     int
	lastCacheCreationTokens int
	lastReasoningTokens     int

	// tcState tracks tool call state across stream chunks.
	tcState toolCallState

	// transformedBytes tracks accumulated output size across the stream.
	// When it exceeds maxTransformedBytes, discarding is set to true.
	transformedBytes uint64
	// maxTransformedBytes caps the accumulated transformed output to
	// prevent memory exhaustion from malicious or buggy upstreams.
	maxTransformedBytes int
	// warnedAtThreshold tracks whether the 80% threshold warning was logged.
	warnedAtThreshold bool
	// discarding indicates output size limit was exceeded; new events are
	// dropped except [DONE] which passes through for clean stream end.
	discarding bool

	// suppressCutError, when true, prevents the interim gateway_error + [DONE]
	// injection for a genuine mid-generation cut (content seen, no finish_reason,
	// no [DONE]). Used by the stream continuation mechanism so the proxy can
	// resume the stream instead of terminating it. When suppressed, the reader
	// ends with a plain EOF; the proxy decides whether to continue or terminate.
	suppressCutError bool
}

type pendingToolCall struct {
	id   string
	args string
}

type toolCallState struct {
	mu          sync.RWMutex
	seenIndices map[int]bool
	pending     map[int]*pendingToolCall
	// seenIDs tracks upstream tool-call IDs already claimed by an index in
	// this stream, so duplicates on other indexes are deconflicted
	// deterministically (NENYA-48).
	seenIDs map[string]bool
}

// pendingToolCallCount returns the number of tool-call entries still
// buffered waiting for their name chunk. Used for end-of-stream loss
// telemetry.
func (r *SSETransformingReader) pendingToolCallCount() int {
	r.tcState.mu.RLock()
	defer r.tcState.mu.RUnlock()
	return len(r.tcState.pending)
}

func newToolCallState() toolCallState {
	return toolCallState{
		seenIndices: make(map[int]bool),
		pending:     make(map[int]*pendingToolCall),
		seenIDs:     make(map[string]bool),
	}
}

// NewSSETransformingReader creates a new reader that transforms SSE data lines using the provided transformer.
// If ctx is nil, context.Background() is used as a safe default.
func NewSSETransformingReader(src io.Reader, transformer ResponseTransformer, ctx context.Context) *SSETransformingReader {
	if ctx == nil {
		ctx = context.Background()
	}
	poolBuf := getStreamBuffer()
	reader := &SSETransformingReader{
		src:                 src,
		scanner:             bufio.NewScanner(src),
		transformer:         transformer,
		ctx:                 ctx,
		tcState:             newToolCallState(),
		poolBuf:             poolBuf,
		maxTransformedBytes: DefaultMaxTransformedEventBytes,
		warnedAtThreshold:   false,
	}
	reader.scanner.Buffer(*poolBuf, SSEScannerMaxBuf)
	return reader
}

// SetOnUsage sets a callback that receives token usage statistics from the stream.
// The callback is invoked from the same goroutine that calls Read().
// The callback must not access the reader instance concurrently, as internal
// state (last* token fields) is updated without mutex protection.
func (r *SSETransformingReader) SetOnUsage(cb UsageCallback) {
	r.onUsage = cb
}

// SetStreamFilter sets a stream filter for content filtering.
func (r *SSETransformingReader) SetStreamFilter(sf *StreamFilter) {
	r.streamFilter = sf
}

// SetExfilGuard sets the output egress guard for URL policy enforcement.
func (r *SSETransformingReader) SetExfilGuard(g *ExfilGuard) {
	r.exfilGuard = g
}

// SetStreamEntropyFilter sets an entropy filter for stream content.
func (r *SSETransformingReader) SetStreamEntropyFilter(ef *StreamEntropyFilter) {
	r.streamEntropyFilter = ef
}

// SetOnContent sets a callback that receives content chunks from the stream.
func (r *SSETransformingReader) SetOnContent(cb ContentCallback) {
	r.onContent = cb
}

// SetOnThinking sets a callback that receives thinking state changes.
// The callback is invoked when a stream starts or ends emitting reasoning/thinking tokens.
func (r *SSETransformingReader) SetOnThinking(cb ThinkingCallback) {
	r.onThinking = cb
}

func (r *SSETransformingReader) SetObserver(obs SSEObserver) {
	r.observer = obs
}

// SetLogger sets a logger for the transforming reader. Used for warning
// messages about malformed SSE data.
// NOTE: Must be called before the reader is used (before any Read calls).
func (r *SSETransformingReader) SetLogger(logger *slog.Logger) {
	r.logger = logger
}

// ResetCounters resets the transformed byte counter and discarding state.
// Used when a transformer is reused across multiple requests to prevent
// state leakage between streams. Currently readers are created per-request,
// so this is reserved for future reader-pooling implementations.
func (r *SSETransformingReader) ResetCounters() {
	r.transformedBytes = 0
	r.discarding = false
	r.warnedAtThreshold = false
	r.sawContent = false
	r.sawFinishReason = false
	r.sawDone = false
	r.injectedError = false
}

// SetSuppressCutError disables the interim gateway_error + [DONE] injection
// for a genuine mid-generation cut (content seen, no finish_reason, no [DONE]).
// When suppressed, the reader ends with a plain EOF; the calling proxy layer
// decides whether to resume the stream instead of terminating it.
func (r *SSETransformingReader) SetSuppressCutError(v bool) {
	r.suppressCutError = v
}

// trackStreamProgress marks whether assistant content or a completion
// signal has been observed in the stream. Used together with SawDone to
// classify no-[DONE] endings: finish_reason without DONE is benign truncation;
// content without finish_reason is a genuine cut.
func (r *SSETransformingReader) trackStreamProgress(transformed []byte) {
	if r.sawContent && r.sawFinishReason {
		return
	}
	data := bytes.TrimSpace(transformed)
	if bytes.HasPrefix(data, []byte("data: ")) {
		data = bytes.TrimSpace(data[6:])
	}
	if len(data) == 0 || data[0] != '{' {
		return
	}
	parsed := ParseSSEChunk(data)
	if parsed == nil {
		return
	}
	content := ExtractDeltaContentFromMap(parsed)
	if content != "" {
		r.sawContent = true
	}
	if !r.sawContent && hasReasoningDelta(parsed) {
		r.sawContent = true
	}
	if !r.sawFinishReason {
		fr := ExtractFinishReasonFromMap(parsed)
		if fr != "" {
			r.sawFinishReason = true
		}
	}
}

// hasReasoningDelta reports whether an OpenAI-format chunk carries a non-empty
// reasoning_content delta (e.g. DeepSeek/Zai thinking streams). Such chunks
// indicate the model is generating even before visible content arrives.
func hasReasoningDelta(chunk map[string]interface{}) bool {
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return false
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return false
	}
	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return false
	}
	rc, _ := delta["reasoning_content"].(string)
	return rc != ""
}

// SetMaxTransformedBytes sets the maximum allowed accumulated transformed output.
// If zero or negative, resets to the default (50MB).
func (r *SSETransformingReader) SetMaxTransformedBytes(maxBytes int) {
	if maxBytes > 0 {
		r.maxTransformedBytes = maxBytes
	} else {
		r.maxTransformedBytes = DefaultMaxTransformedEventBytes
	}
}

// SawDone returns true if the reader has observed a data: [DONE] event.
func (r *SSETransformingReader) SawDone() bool {
	return r.sawDone
}

// SawContent returns true if any assistant content chunk has been forwarded
// to the client. Used together with SawFinishReason to classify stream
// endings that lack a [DONE] marker.
func (r *SSETransformingReader) SawContent() bool {
	return r.sawContent
}

// SawFinishReason returns true if the model signaled a completion
// (non-null finish_reason) within the stream. A stream ending without [DONE]
// after a finish_reason is benign truncation; without one it is a genuine
// mid-generation cut.
func (r *SSETransformingReader) SawFinishReason() bool {
	return r.sawFinishReason
}

// SuppressCutError returns true if the reader is suppressing the interim
// gateway_error injection for a genuine mid-generation cut.
func (r *SSETransformingReader) SuppressCutError() bool {
	return r.suppressCutError
}

// InjectedError returns true if the reader replaced the upstream's terminal
// with a synthesized gateway_error frame (empty stream, mid-generation cut,
// oversize line, or size-limit discard). Such streams are sanitized
// truncations: they must never be persisted to the response cache (NENYA-27)
// because replaying them would serve a dead upstream's partial output.
func (r *SSETransformingReader) InjectedError() bool {
	return r.injectedError
}

// getTransformedLine returns the transformed line, using pooled buffers when possible.
func (r *SSETransformingReader) getTransformedLine(line []byte) []byte {
	if len(line) <= maxPooledBufSize {
		buf := GetLineCopyBuffer()
		*buf = (*buf)[:len(line)]
		copy(*buf, line)
		transformed := r.transformLine(*buf)
		PutLineCopyBuffer(buf)
		return transformed
	}
	lineCopy := make([]byte, len(line))
	copy(lineCopy, line)
	return r.transformLine(lineCopy)
}

// safePercentage calculates percentage with overflow protection.
func safePercentage(part, total uint64) uint64 {
	if total == 0 {
		return 0
	}
	if part < math.MaxUint64/100 {
		return part * 100 / total
	}
	return 100
}

func (r *SSETransformingReader) Read(p []byte) (int, error) {
	if r.ctx != nil {
		select {
		case <-r.ctx.Done():
			buf := r.poolBuf
			r.poolBuf = nil
			putStreamBuffer(buf)
			return 0, r.ctx.Err()
		default:
		}
	}

	if r.pos < len(r.buffer) {
		n := copy(p, r.buffer[r.pos:])
		r.pos += n
		if r.pos >= len(r.buffer) {
			r.buffer = nil
			r.pos = 0
		}
		return n, nil
	}

	if r.err != nil {
		buf := r.poolBuf
		r.poolBuf = nil
		putStreamBuffer(buf)
		return 0, r.err
	}

	if r.discarding && !errors.Is(r.err, ErrTransformedSizeExceeded) {
		r.injectErrorBuffer("transformed output exceeded maximum size")
		r.err = ErrTransformedSizeExceeded
		return r.Read(p)
	}

	if !r.scanner.Scan() {
		r.handleScannerDone()
		return r.drainAfterScanDone(p)
	}

	for {
		transformed := r.getTransformedLine(r.scanner.Bytes())

		if transformed == nil {
			if r.anyFilterBlocked() {
				// The last scanned chunk flipped a filter to blocked:
				// surface the specific error instead of a generic
				// truncated-stream gateway error.
				r.err = r.blockedStreamError()
				buf := r.poolBuf
				r.poolBuf = nil
				putStreamBuffer(buf)
				return 0, r.err
			}
			if !r.scanner.Scan() {
				r.handleScannerDone()
				return r.drainAfterScanDone(p)
			}
			continue
		}

		if r.anyFilterBlocked() {
			r.err = r.blockedStreamError()
			buf := r.poolBuf
			r.poolBuf = nil
			putStreamBuffer(buf)
			return 0, r.err
		}

		if !bytes.HasSuffix(transformed, []byte("\n")) {
			transformed = append(transformed, '\n')
		}

		r.trackTransformedSize(transformed)
		r.trackStreamProgress(transformed)

		r.buffer = transformed
		r.pos = 0

		return r.Read(p)
	}
}

// trackTransformedSize accumulates the transformed output size and sets the
// discarding flag when the total exceeds maxTransformedBytes.
func (r *SSETransformingReader) trackTransformedSize(transformed []byte) {
	blen := uint64(len(transformed))
	if r.transformedBytes > ^uint64(0)-blen {
		r.transformedBytes = ^uint64(0)
	} else {
		r.transformedBytes += blen
	}
	if r.logger != nil && !r.warnedAtThreshold && r.maxTransformedBytes > 0 {
		threshold := uint64(r.maxTransformedBytes) / 10 * 8
		if r.transformedBytes >= threshold {
			r.warnedAtThreshold = true
			r.logger.Warn("transformed SSE output approaching size limit",
				"accumulated", r.transformedBytes,
				"limit", r.maxTransformedBytes,
				"pct", safePercentage(r.transformedBytes, uint64(r.maxTransformedBytes)))
		}
	}
	if r.transformedBytes > uint64(r.maxTransformedBytes) && !r.discarding {
		if r.logger != nil {
			r.logger.Warn("transformed output exceeded size limit, will discard subsequent events",
				"limit", r.maxTransformedBytes, "accumulated", r.transformedBytes)
		}
		r.discarding = true
	}
}

func (r *SSETransformingReader) transformLine(line []byte) []byte {
	if r.discarding {
		// Allow [DONE] to pass through so the stream can end cleanly
		if bytes.HasPrefix(line, []byte("data: ")) && bytes.Equal(bytes.TrimSpace(line[6:]), []byte("[DONE]")) {
			r.sawDone = true
			return line
		}
		return nil
	}

	if len(line) == 0 {
		return line
	}

	if !bytes.HasPrefix(line, []byte("data: ")) {
		result := r.transformNonSSELine(line)
		if result == nil {
			return nil
		}
		return result
	}

	result := r.transformSSEData(line)
	if result == nil {
		return nil
	}
	return result
}

// handleScannerDone is called when the scanner has no more input.
// It sets r.err and optionally injects a terminal event into r.buffer.
// Classification of a clean EOF (scanner.Err() == nil, no [DONE]):
//   - finish_reason seen: benign truncation → inject silent [DONE], no error.
//   - no content at all: empty stream → inject gateway_error + [DONE].
//   - content seen but no finish_reason: genuine mid-generation cut → keep
//     interim gateway_error injection (Phase 1 continuation intercepts this
//     via SawContent/SawFinishReason).
func (r *SSETransformingReader) handleScannerDone() {
	// Terminal-state matrix hygiene (NENYA-46): argument chunks buffered
	// waiting for a name that never arrived are silently dropped by
	// design (they were never forwarded). Surface the loss — a provider
	// emitting args-before-name then terminating has lost those tool
	// calls for the client, and the log is the only trace.
	if n := r.pendingToolCallCount(); n > 0 {
		slog.Warn("stream ended with unflushed tool_call argument buffers (name chunk never arrived)",
			"pending", n,
			"sawDone", r.sawDone,
			"sawFinishReason", r.sawFinishReason)
	}
	switch r.scanner.Err() {
	case nil:
		switch {
		case r.sawDone:
			// Normal end.
		case r.sawFinishReason:
			slog.Debug("SSE scanner ended without [DONE] after finish_reason, injecting silent [DONE]",
				"has_transformer", r.transformer != nil)
			r.injectDoneOnly()
		case !r.sawContent:
			slog.Warn("SSE scanner ended without [DONE] marker and no content, injecting gateway_error",
				"has_transformer", r.transformer != nil)
			r.injectErrorBuffer("upstream stream ended without [DONE]")
		default:
			if r.suppressCutError {
				slog.Debug("SSE scanner ended without [DONE] mid-generation, cut error suppressed for continuation",
					"has_transformer", r.transformer != nil,
					"saw_content", r.sawContent)
				break
			}
			slog.Warn("SSE scanner ended without [DONE] marker mid-generation, injecting gateway_error",
				"has_transformer", r.transformer != nil,
				"saw_content", r.sawContent)
			r.injectErrorBuffer("upstream stream ended without [DONE]")
		}
		r.err = io.EOF
	case bufio.ErrTooLong:
		slog.Warn("SSE scanner error: line too long, injecting gateway_error")
		r.injectErrorBuffer("upstream SSE line exceeded maximum scanner buffer")
		r.err = r.scanner.Err()
	default:
		slog.Warn("SSE scanner error", "err", r.scanner.Err())
		r.err = r.scanner.Err()
	}
}

// injectDoneOnly buffers a bare [DONE] event without a gateway_error. Used for
// benign stream endings where the model signaled completion but the upstream
// omitted the [DONE] marker.
func (r *SSETransformingReader) injectDoneOnly() {
	r.buffer = []byte("data: [DONE]\n\n")
	r.pos = 0
}

// injectErrorBuffer creates a gateway_error SSE event + [DONE] and places it
// in r.buffer so the client receives the error before EOF.
func (r *SSETransformingReader) injectErrorBuffer(message string) {
	r.injectedError = true
	slog.Warn("injecting gateway_error into stream", "message", message, "sawDone", r.sawDone)
	errPayload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "gateway_error",
		},
	})
	if err != nil {
		slog.Error("failed to marshal error payload", "error", err, "message", message)
		errPayload = []byte(`{"error":{"message":"internal error","type":"gateway_error"}}`)
	}
	r.buffer = append(append([]byte("data: "), errPayload...), []byte("\n\ndata: [DONE]\n\n")...)
	r.pos = 0
	if r.observer != nil {
		var errMap map[string]any
		if err := json.Unmarshal(errPayload, &errMap); err != nil {
			slog.Error("failed to unmarshal error payload for observer", "error", err)
			r.observer.OnSSEEvent(SSEEvent{Type: "error", Data: map[string]any{"message": message, "type": "gateway_error"}, GatewayInjected: true})
		} else {
			r.observer.OnSSEEvent(SSEEvent{Type: "error", Data: errMap, GatewayInjected: true})
		}
	}
}

// drainAfterScanDone returns any injected error buffer data before signaling
// the terminal error. This ensures error+[DONE] events reach the client
// when the upstream stream ends without [DONE].
func (r *SSETransformingReader) drainAfterScanDone(p []byte) (int, error) {
	if r.pos < len(r.buffer) {
		n := copy(p, r.buffer[r.pos:])
		r.pos += n
		if r.pos >= len(r.buffer) {
			r.buffer = nil
			r.pos = 0
		}
		return n, nil
	}
	if r.observer != nil && !r.closed {
		r.closed = true
		r.observer.OnStreamClose(r.err)
	}
	buf := r.poolBuf
	r.poolBuf = nil
	putStreamBuffer(buf)
	return 0, r.err
}

func (r *SSETransformingReader) transformSSEData(line []byte) []byte {
	origData := bytes.TrimPrefix(line, []byte("data: "))

	if len(origData) == 0 {
		r.notifySSEObserver(line, nil, "keepalive")
		return line
	}
	if bytes.Equal(origData, []byte("[DONE]")) {
		r.sawDone = true
		if synthLine, ok := r.synthesizeTerminalFinish(); ok {
			r.notifySSEObserver(synthLine, ParseSSEChunk(synthLine[6:]), "")
			r.notifySSEObserver(line, nil, "done")
			return append(append(append([]byte{}, synthLine...), []byte("\n\n")...), line...)
		}
		r.notifySSEObserver(line, nil, "done")
		return line
	}

	data := origData
	parsed := r.tryParseJSON(data)

	// Upstream error payloads routed through a healthy 200 stream (e.g. an
	// aggregator whose provider leg failed after the stream opened) are tagged
	// with "error" so observers can record circuit-breaker failures. The error
	// line itself is still forwarded to the client unchanged below.
	if IsStreamErrorPayload(parsed) {
		r.notifySSEObserver(line, parsed, "error")
	}

	parsed, filtersMutated := r.applyStreamFilters(parsed)
	if (r.streamFilter != nil && r.streamFilter.IsBlocked()) ||
		(r.exfilGuard != nil && r.exfilGuard.IsBlocked()) {
		// A filter blocked this exact chunk: suppress the violating
		// line; the stream terminates with the filter-specific error
		// next Read. Nil parsed from malformed JSON still falls through
		// to passthrough below.
		return nil
	}
	if filtersMutated {
		// Filter rewrites live in the parsed map: re-encode so the wire
		// output (transformer input and passthrough) carries them.
		if reencoded, err := json.Marshal(parsed); err == nil {
			data = reencoded
		}
	}

	data = r.applyContentFilters(data, parsed)

	r.callUsageAndContentCallbacks(parsed)

	if r.transformer == nil {
		result := r.handleNoTransformer(parsed, data, origData, line)
		if result == nil {
			return nil
		}
		return result
	}

	result := r.handleWithTransformer(parsed, data, origData, line)
	if result == nil {
		return nil
	}
	return result
}

// notifySSEObserver forwards a parsed SSE event to the registered observer.
// Events already classified and notified as upstream errors (eventType "error")
// are suppressed here so observers count each error exactly once.
func (r *SSETransformingReader) notifySSEObserver(line []byte, parsed map[string]interface{}, eventType string) {
	if r.observer == nil {
		return
	}
	// Upstream error payloads are already notified with Type "error" when
	// parsed; suppress the duplicate downstream notify so observers count each
	// error event exactly once.
	if eventType == "" && IsStreamErrorPayload(parsed) {
		return
	}
	r.observer.OnSSEEvent(SSEEvent{Type: eventType, Raw: line, Data: parsed})
}

// synthesizeTerminalFinish implements the [DONE]-without-finish_reason cell
// of the terminal-state matrix (NENYA-46): an OpenAI passthrough stream that
// streamed content but never signaled completion leaves the client message
// open when [DONE] arrives — clients keying on finish_reason (tool-call
// reconciliation, turn finalization) hang or reconcile incorrectly. A
// synthetic finish_reason:"stop" chunk is emitted ahead of [DONE] so the
// message closes protocol-correctly.
//
// Deliberately narrow: only pure passthrough (no format transformer — the
// client format would be unknown), only when content was actually streamed,
// never when a finish_reason was seen, and never in discard mode (the early
// [DONE] passthrough branch bypasses this path entirely).
func (r *SSETransformingReader) synthesizeTerminalFinish() ([]byte, bool) {
	if r.transformer != nil || !r.sawContent || r.sawFinishReason {
		return nil, false
	}
	r.sawFinishReason = true
	synth := []byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	if r.logger != nil {
		r.logger.Debug("synthesized finish_reason before [DONE] (stream ended with open message)")
	}
	return synth, true
}

func (r *SSETransformingReader) tryParseJSON(data []byte) map[string]interface{} {
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return nil
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		if r.logger != nil {
			r.logger.Warn("malformed JSON in SSE data line",
				"err", err,
				"data_len", len(data))
		}
		return nil
	}
	return parsed
}

// applyStreamFilters runs the stream-side content filters over one
// parsed chunk, reporting whether any filter rewrote it (callers must
// re-encode; mutations live only in the map). A nil map means a filter
// blocked the chunk.
func (r *SSETransformingReader) applyStreamFilters(parsed map[string]interface{}) (map[string]interface{}, bool) {
	if parsed == nil {
		return parsed, false
	}
	mutated := false

	if r.streamFilter != nil && !r.streamFilter.IsBlocked() {
		var action FilterAction
		parsed, action = applyStreamFilter(parsed, r.streamFilter)
		if r.streamFilter.IsBlocked() {
			return nil, false
		}
		mutated = mutated || action == ActionRedact
	}

	if r.exfilGuard != nil && !r.exfilGuard.IsBlocked() {
		var action FilterAction
		parsed, action = applyExfilGuard(parsed, r.exfilGuard)
		if r.exfilGuard.IsBlocked() {
			return nil, false
		}
		mutated = mutated || action == ActionRedact
	}

	if r.streamEntropyFilter != nil {
		var changed bool
		parsed, changed = applyEntropyFilter(parsed, r.streamEntropyFilter)
		mutated = mutated || changed
	}

	return parsed, mutated
}

// applyStreamFilter runs the secret/policy filter over one SSE delta,
// reporting the resulting action so callers can detect rewrites.
func applyStreamFilter(parsed map[string]interface{}, filter *StreamFilter) (map[string]interface{}, FilterAction) {
	content := ExtractDeltaContentFromMap(parsed)
	if content == "" {
		return parsed, ActionPass
	}
	redacted, action, _ := filter.FilterContent(content)
	if action == ActionBlock {
		return nil, ActionBlock
	}
	if action == ActionRedact && redacted != content {
		parsed = copyMap(parsed)
		_ = ReplaceDeltaContentMap(parsed, redacted)
	}
	return parsed, action
}

// anyFilterBlocked reports whether a stream-side filter has terminated
// the stream.
func (r *SSETransformingReader) anyFilterBlocked() bool {
	return (r.streamFilter != nil && r.streamFilter.IsBlocked()) ||
		(r.exfilGuard != nil && r.exfilGuard.IsBlocked())
}

// blockedStreamError returns the specific error for whichever filter
// terminated the stream (exfil guard takes precedence; both wrap as
// stream-block errors for generic handling).
func (r *SSETransformingReader) blockedStreamError() error {
	if r.exfilGuard != nil && r.exfilGuard.IsBlocked() {
		return ErrExfilBlocked
	}
	return ErrStreamBlocked
}

// applyExfilGuard runs the URL egress policy over one SSE delta in
// either wire format (OpenAI or Anthropic). Stripped content replaces
// the delta field in place; a block verdict nils the chunk so the
// reader raises ErrExfilBlocked (the guard itself flips to blocked).
func applyExfilGuard(parsed map[string]interface{}, guard *ExfilGuard) (map[string]interface{}, FilterAction) {
	content := extractExfilContent(parsed)
	if content == "" {
		return parsed, ActionPass
	}
	rewritten, action, _ := guard.FilterContent(content)
	switch action {
	case ActionBlock:
		return nil, ActionBlock
	case ActionRedact:
		parsed = copyMap(parsed)
		if !setExfilContent(parsed, rewritten) {
			return parsed, ActionPass
		}
	}
	return parsed, action
}

// applyEntropyFilter runs the entropy redactor over one SSE delta,
// reporting whether it rewrote the chunk.
func applyEntropyFilter(parsed map[string]interface{}, filter *StreamEntropyFilter) (map[string]interface{}, bool) {
	content := ExtractDeltaContentFromMap(parsed)
	if content == "" {
		return parsed, false
	}
	redacted, action := filter.FilterContent(content)
	if action == ActionRedact && redacted != content {
		parsed = copyMap(parsed)
		ReplaceDeltaContentMap(parsed, redacted)
		return parsed, true
	}
	return parsed, false
}

func copyMap(m map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (r *SSETransformingReader) applyContentFilters(data []byte, parsed map[string]interface{}) []byte {
	// TODO: Implement content filtering if needed (currently no-op)
	return data
}

func (r *SSETransformingReader) callUsageAndContentCallbacks(parsed map[string]interface{}) {
	if parsed == nil {
		return
	}
	if r.onUsage != nil {
		r.extractUsageFromMap(parsed)
	}
	if r.onContent != nil {
		if content := ExtractDeltaContentFromMap(parsed); content != "" {
			r.onContent(content)
		}
	}
	if r.onThinking != nil {
		if active, hasSignal := ExtractThinkingSignal(parsed); hasSignal {
			r.onThinking(active)
		}
	}
}

func (r *SSETransformingReader) handleNoTransformer(parsed map[string]interface{}, data []byte, origData []byte, line []byte) []byte {
	if parsed != nil && normalizeToolCalls(parsed, &r.tcState) {
		if out, err := json.Marshal(parsed); err == nil {
			data = out
		}
	}

	if bytes.Equal(data, origData) {
		r.notifySSEObserver(line, parsed, "")
		return line
	}

	finalLine := append([]byte("data: "), data...)
	r.notifySSEObserver(finalLine, parsed, "")
	return finalLine
}

func (r *SSETransformingReader) handleWithTransformer(parsed map[string]interface{}, data []byte, origData []byte, line []byte) []byte {
	transformed, err := r.transformer.TransformSSEChunk(r.ctx, data)
	if err != nil {
		if errors.Is(err, ErrEventConsumed) {
			r.notifySSEObserver(line, parsed, "consumed")
			return nil
		}
		r.notifySSEObserver(line, parsed, "")
		return line
	}

	if len(transformed) == 0 {
		r.notifySSEObserver(line, parsed, "")
		return line
	}

	transformed = r.applyToolCallNormalization(transformed, &r.tcState)

	if bytes.Equal(transformed, []byte("[DONE]")) {
		r.sawDone = true
		r.notifySSEObserver([]byte("data: [DONE]"), nil, "done")
		return []byte("data: [DONE]")
	}

	if bytes.Equal(transformed, origData) && bytes.Equal(data, origData) {
		if parsed != nil {
			marshaled, err := json.Marshal(parsed)
			if err == nil && !bytes.Equal(marshaled, origData) {
				finalLine := append([]byte("data: "), marshaled...)
				r.notifySSEObserver(finalLine, parsed, "")
				return finalLine
			}
		}
		r.notifySSEObserver(line, parsed, "")
		return line
	}

	finalLine := append([]byte("data: "), transformed...)
	r.notifySSEObserver(finalLine, parsed, "")
	return finalLine
}

func (r *SSETransformingReader) applyToolCallNormalization(transformed []byte, state *toolCallState) []byte {
	if len(transformed) == 0 || transformed[0] != '{' {
		return transformed
	}
	var transformedParsed map[string]interface{}
	if json.Unmarshal(transformed, &transformedParsed) != nil {
		return transformed
	}
	if normalizeToolCalls(transformedParsed, state) {
		if out, err := json.Marshal(transformedParsed); err == nil {
			return out
		}
	}
	return transformed
}

func (r *SSETransformingReader) transformNonSSELine(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return line
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return line
	}
	if r.transformer == nil {
		return line
	}
	transformed, err := r.transformer.TransformSSEChunk(r.ctx, trimmed)
	if err != nil {
		if errors.Is(err, ErrEventConsumed) {
			r.notifySSEObserver(line, nil, "consumed")
			return nil
		}
		return line
	}
	if len(transformed) == 0 || bytes.Equal(transformed, trimmed) {
		return line
	}
	return transformed
}

// ReasoningTokenExtractors lists the known provider spellings for the
// reasoning/thinking token counter inside a usage object (NENYA-33).
// They are alternative spellings of one counter, so the first non-zero
// value wins; zero-token placeholders never register.
var reasoningTokenExtractors = []func(usage map[string]interface{}) int{
	func(u map[string]interface{}) int { return ToInt(u["reasoning_tokens"]) },
	func(u map[string]interface{}) int {
		if d, ok := u["output_tokens_details"].(map[string]interface{}); ok {
			return ToInt(d["reasoning_tokens"]) // OpenAI current shape
		}
		return 0
	},
	func(u map[string]interface{}) int {
		if d, ok := u["completion_tokens_details"].(map[string]interface{}); ok {
			return ToInt(d["reasoning_tokens"]) // legacy OpenAI shape
		}
		return 0
	},
	func(u map[string]interface{}) int { return ToInt(u["completion_reasoning_tokens"]) }, // Anthropic raw
	func(u map[string]interface{}) int { return ToInt(u["thoughtsTokenCount"]) },          // Gemini native
}

// ExtractReasoningTokens pulls the reasoning/thinking token count from a
// usage object across every known provider spelling. Returns 0 when no
// spelling yields a value.
func ExtractReasoningTokens(usage map[string]interface{}) int {
	for _, extract := range reasoningTokenExtractors {
		if n := extract(usage); n > 0 {
			return n
		}
	}
	return 0
}

func (r *SSETransformingReader) extractUsageFromMap(chunk map[string]interface{}) {
	rawUsage, ok := chunk["usage"]
	if !ok || rawUsage == nil {
		return
	}
	usage, ok := rawUsage.(map[string]interface{})
	if !ok || usage == nil {
		return
	}
	completion := ToInt(usage["completion_tokens"])
	prompt := ToInt(usage["prompt_tokens"])
	total := ToInt(usage["total_tokens"])
	cacheHit := ToInt(usage["prompt_cache_hit_tokens"])
	if cacheHit == 0 {
		if promptTokensDetails, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
			cacheHit = ToInt(promptTokensDetails["cached_tokens"])
		}
	}
	cacheMiss := ToInt(usage["prompt_cache_miss_tokens"])
	cacheCreation := ToInt(usage["cache_creation_tokens"])
	reasoning := ExtractReasoningTokens(usage)
	cacheReadInput := ToInt(usage["cache_read_input_tokens"])
	cacheCreationInput := ToInt(usage["cache_creation_input_tokens"])
	if allUsageFieldsZero(completion, prompt, total, cacheHit, cacheMiss, cacheCreation, reasoning) &&
		cacheReadInput == 0 && cacheCreationInput == 0 {
		return
	}
	dCompletion := clampDelta(completion, r.lastCompletionTokens)
	dPrompt := clampDelta(prompt, r.lastPromptTokens)
	dTotal := clampDelta(total, r.lastTotalTokens)
	dCacheHit := clampDelta(cacheHit, r.lastCacheHitTokens)
	dCacheMiss := clampDelta(cacheMiss, r.lastCacheMissTokens)
	dCacheCreation := clampDelta(cacheCreation, r.lastCacheCreationTokens)
	dReasoning := clampDelta(reasoning, r.lastReasoningTokens)
	dCacheReadInput := clampDelta(cacheReadInput, 0)
	dCacheCreationInput := clampDelta(cacheCreationInput, 0)
	if allDeltasZero(dCompletion, dPrompt, dTotal, dCacheHit, dCacheMiss, dCacheCreation, dReasoning) &&
		dCacheReadInput == 0 && dCacheCreationInput == 0 {
		return
	}
	r.lastCompletionTokens = completion
	r.lastPromptTokens = prompt
	r.lastTotalTokens = total
	r.lastCacheHitTokens = cacheHit
	r.lastCacheMissTokens = cacheMiss
	r.lastCacheCreationTokens = cacheCreation
	r.lastReasoningTokens = reasoning
	r.onUsage(UsageData{
		CompletionTokens:         dCompletion,
		PromptTokens:             dPrompt,
		TotalTokens:              dTotal,
		CacheHitTokens:           dCacheHit,
		CacheMissTokens:          dCacheMiss,
		CacheCreationTokens:      dCacheCreation,
		ReasoningTokens:          dReasoning,
		CacheReadInputTokens:     dCacheReadInput,
		CacheCreationInputTokens: dCacheCreationInput,
	})
}

func allUsageFieldsZero(completion, prompt, total, cacheHit, cacheMiss, cacheCreation, reasoning int) bool {
	return completion == 0 && prompt == 0 && total == 0 && cacheHit == 0 && cacheMiss == 0 && cacheCreation == 0 && reasoning == 0
}

func allDeltasZero(dCompletion, dPrompt, dTotal, dCacheHit, dCacheMiss, dCacheCreation, dReasoning int) bool {
	return dCompletion == 0 && dPrompt == 0 && dTotal == 0 && dCacheHit == 0 && dCacheMiss == 0 && dCacheCreation == 0 && dReasoning == 0
}

// clampDelta computes a non-negative delta between current and last value.
// Returns 0 when current < last (e.g. counter reset), preventing underflow.
func clampDelta(current, last int) int {
	d := current - last
	if d < 0 {
		return 0
	}
	return d
}

// ToInt converts an interface{} value to int, handling float64, string, and int types.
// Returns 0 for unsupported types or on parse error.
func ToInt(v interface{}) int {
	switch n := v.(type) {
	case float64:
		if n > math.MaxInt || n < math.MinInt {
			return 0
		}
		return int(n)
	case int:
		return n
	case string:
		i, err := strconv.Atoi(n)
		if err != nil {
			return 0
		}
		return i
	default:
		return 0
	}
}

// normalizeToolCalls processes tool_calls deltas in a streaming response chunk.
// It normalizes missing tool_call IDs, buffers args chunks that arrive before names,
// and merges pending data when names arrive. Returns true if the chunk was mutated.
func normalizeToolCalls(chunk map[string]interface{}, state *toolCallState) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	choices, ok := chunk["choices"].([]interface{})
	if !ok {
		return false
	}
	mutated := false
	for _, c := range choices {
		choice, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]interface{})
		if !ok {
			continue
		}
		tcs, ok := delta["tool_calls"].([]interface{})
		if !ok {
			continue
		}

		keep, mutatedInDelta := processToolCallDelta(tcs, state)
		if mutatedInDelta {
			mutated = true
		}
		if len(keep) != len(tcs) {
			mutated = true
			if len(keep) == 0 {
				delete(delta, "tool_calls")
			} else {
				delta["tool_calls"] = keep
			}
		}
	}
	return mutated
}

// processToolCallDelta normalizes a single tool_calls delta array. It buffers
// argument chunks that arrive before the tool name and merges pending data
// when names arrive. Returns the filtered slice and whether it was mutated.
//
// Identity rules (NENYA-48): the call index falls back to the ARRAY POSITION
// when the upstream omits it (never 0-for-all, which collides across calls
// in one delta); invalid, empty, or oversized IDs are replaced with the
// deterministic synthetic ID for that index; and an ID already claimed by a
// different index in the same stream is deterministically deconflicted the
// same way — same input, same output, across requests.
func processToolCallDelta(tcs []interface{}, state *toolCallState) ([]interface{}, bool) {
	keep := make([]interface{}, 0, len(tcs))
	mutated := false

	for pos, tcRaw := range tcs {
		tc, ok := tcRaw.(map[string]interface{})
		if !ok {
			continue
		}
		rawIdx, hasIdx := tc["index"]
		var idx int
		if hasIdx {
			idx = util.ParseToolCallIndex(rawIdx, pos)
		} else {
			idx = pos
		}

		if state.seenIndices[idx] {
			keep = append(keep, tc)
			continue
		}

		mutated = normalizeToolCallID(tc, idx, state) || mutated

		fn, hasFn := tc["function"]
		tcID, _ := tc["id"].(string)
		fnNameStr := extractToolCallName(fn, hasFn, &mutated, tcID)
		fnArgsStr := extractToolCallArgs(fn, hasFn)

		if fnNameStr != "" {
			handleToolCallWithName(tc, idx, tcID, fnArgsStr, state, &mutated)
			state.seenIndices[idx] = true
			keep = append(keep, tc)
			continue
		}

		if fnArgsStr != "" && fnArgsStr != "{}" {
			bufferPendingToolCall(idx, tcID, fnArgsStr, state, &mutated)
		}
	}
	return keep, mutated
}

// normalizeToolCallID enforces deterministic tool-call identity on a new
// (not-yet-seen) call index: invalid/empty/oversized upstream IDs and IDs
// already claimed by another index are replaced with the synthetic ID for
// the call index. Returns whether the chunk was mutated.
func normalizeToolCallID(tc map[string]interface{}, idx int, state *toolCallState) bool {
	if !util.ValidToolCallID(tc["id"]) {
		tc["id"] = util.SyntheticToolCallID(idx)
		return true
	}
	id, _ := tc["id"].(string)
	if state.seenIDs[id] {
		// Duplicate upstream ID on a different index: keep the first
		// claimant, deterministically re-key the later one. Synthesizing
		// from the index keeps the mapping stable across requests.
		tc["id"] = util.SyntheticToolCallID(idx)
		return true
	}
	state.seenIDs[id] = true
	return false
}

func handleToolCallWithName(tc map[string]interface{}, idx int, tcID string, fnArgsStr string, state *toolCallState, mutated *bool) {
	if p, ok := state.pending[idx]; ok {
		mergePendingToolCall(tc, idx, tcID, fnArgsStr, p, mutated)
		delete(state.pending, idx)
		slog.Debug("merged pending tool_call data on name arrival",
			"index", idx,
			"pending_args_len", len(p.args),
			"tool_call_id", tcID,
		)
	}
}

func mergePendingToolCall(tc map[string]interface{}, idx int, tcID string, fnArgsStr string, pending *pendingToolCall, mutated *bool) {
	if (tcID == "" || util.IsSyntheticToolCallID(tcID)) && pending.id != "" {
		tc["id"] = pending.id
	}
	fn, ok := tc["function"].(map[string]interface{})
	if !ok {
		return
	}
	// "{}" is a JSON placeholder with no real data; use pending args as-is.
	if fnArgsStr == "" || fnArgsStr == "{}" {
		fn["arguments"] = pending.args
	} else {
		fn["arguments"] = pending.args + fnArgsStr
	}
	*mutated = true
}

func bufferPendingToolCall(idx int, tcID string, fnArgsStr string, state *toolCallState, mutated *bool) {
	state.pending[idx] = &pendingToolCall{
		id:   tcID,
		args: fnArgsStr,
	}
	*mutated = true
	slog.Debug("buffered tool_call entry missing name, waiting for name chunk",
		"index", idx,
		"buffered_args_len", len(fnArgsStr),
		"tool_call_id", tcID,
	)
}

func extractToolCallName(fn interface{}, hasFn bool, mutated *bool, tcID string) string {
	if !hasFn || fn == nil {
		return ""
	}
	fnMap, ok := fn.(map[string]interface{})
	if !ok {
		return ""
	}
	fnNameRaw := fnMap["name"]
	switch fnName := fnNameRaw.(type) {
	case string:
		return fnName
	case nil:
		return ""
	default:
		coerced := fmt.Sprintf("%v", fnNameRaw)
		fnMap["name"] = coerced
		*mutated = true
		slog.Debug("coerced non-string function.name to string",
			"coerced_value", coerced,
			"original_type", reflect.TypeOf(fnNameRaw).String(),
			"tool_call_id", tcID,
		)
		return coerced
	}
}

func extractToolCallArgs(fn interface{}, hasFn bool) string {
	if !hasFn || fn == nil {
		return ""
	}
	fnMap, ok := fn.(map[string]interface{})
	if !ok {
		return ""
	}
	args, _ := fnMap["arguments"].(string)
	return args
}
