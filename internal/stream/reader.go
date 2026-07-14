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
// Parameters:
//   - completionTokens: delta output tokens since last callback (always >= 0)
//   - promptTokens: delta input tokens since last callback (always >= 0)
//   - totalTokens: delta total tokens since last callback (always >= 0)
//   - cacheHitTokens: delta cache hit tokens since last callback (always >= 0)
//   - cacheMissTokens: delta cache miss tokens since last callback (always >= 0)
//   - cacheCreationTokens: delta cache creation tokens since last callback (always >= 0)
//   - reasoningTokens: delta reasoning tokens since last callback (always >= 0)
type UsageCallback func(completionTokens, promptTokens, totalTokens, cacheHitTokens, cacheMissTokens, cacheCreationTokens, reasoningTokens int)

type ContentCallback func(content string)

// SSEObserver receives notifications about SSE events during streaming.
// Observers are called after transformation, so they see what the client receives.
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
}

// SSETransformingReader reads from an SSE source and transforms SSE data lines.
type SSETransformingReader struct {
	src                 io.Reader
	scanner             *bufio.Scanner
	transformer         ResponseTransformer
	onUsage             UsageCallback
	onContent           ContentCallback
	observer            SSEObserver
	streamFilter        *StreamFilter
	streamEntropyFilter *StreamEntropyFilter
	buffer              []byte
	pos                 int
	err                 error
	closed              bool
	sawDone             bool
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
}

type pendingToolCall struct {
	id   string
	args string
}

type toolCallState struct {
	mu          sync.RWMutex
	seenIndices map[int]bool
	pending     map[int]*pendingToolCall
}

func newToolCallState() toolCallState {
	return toolCallState{
		seenIndices: make(map[int]bool),
		pending:     make(map[int]*pendingToolCall),
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

// SetStreamEntropyFilter sets an entropy filter for stream content.
func (r *SSETransformingReader) SetStreamEntropyFilter(ef *StreamEntropyFilter) {
	r.streamEntropyFilter = ef
}

// SetOnContent sets a callback that receives content chunks from the stream.
func (r *SSETransformingReader) SetOnContent(cb ContentCallback) {
	r.onContent = cb
}

// SetObserver sets an observer for SSE events during streaming.
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
			if !r.scanner.Scan() {
				r.handleScannerDone()
				return r.drainAfterScanDone(p)
			}
			continue
		}

		if r.streamFilter != nil && r.streamFilter.IsBlocked() {
			r.err = ErrStreamBlocked
			buf := r.poolBuf
			r.poolBuf = nil
			putStreamBuffer(buf)
			return 0, r.err
		}

		if !bytes.HasSuffix(transformed, []byte("\n")) {
			transformed = append(transformed, '\n')
		}

		r.trackTransformedSize(transformed)

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
		threshold := uint64(r.maxTransformedBytes) * 8 / 10
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
// It sets r.err and optionally injects error+[DONE] into r.buffer.
func (r *SSETransformingReader) handleScannerDone() {
	switch r.scanner.Err() {
	case nil:
		if !r.sawDone {
			slog.Warn("SSE scanner ended without [DONE] marker, injecting gateway_error",
				"has_transformer", r.transformer != nil,
				"scanner_error", r.scanner.Err())
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

// injectErrorBuffer creates a gateway_error SSE event + [DONE] and places it
// in r.buffer so the client receives the error before EOF.
func (r *SSETransformingReader) injectErrorBuffer(message string) {
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
			r.observer.OnSSEEvent(SSEEvent{Type: "error", Data: map[string]any{"message": message, "type": "gateway_error"}})
		} else {
			r.observer.OnSSEEvent(SSEEvent{Type: "error", Data: errMap})
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
		r.notifySSEObserver(line, nil, "done")
		return line
	}

	data := origData
	parsed := r.tryParseJSON(data)

	parsed = r.applyStreamFilters(parsed)
	if r.streamFilter != nil && r.streamFilter.IsBlocked() {
		return line
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

func (r *SSETransformingReader) notifySSEObserver(line []byte, parsed map[string]interface{}, eventType string) {
	if r.observer != nil {
		r.observer.OnSSEEvent(SSEEvent{Type: eventType, Raw: line, Data: parsed})
	}
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

func (r *SSETransformingReader) applyStreamFilters(parsed map[string]interface{}) map[string]interface{} {
	if parsed == nil {
		return parsed
	}

	if r.streamFilter != nil && !r.streamFilter.IsBlocked() {
		parsed = applyStreamFilter(parsed, r.streamFilter)
		if r.streamFilter.IsBlocked() {
			return nil
		}
	}

	if r.streamEntropyFilter != nil {
		parsed = applyEntropyFilter(parsed, r.streamEntropyFilter)
	}

	return parsed
}

func applyStreamFilter(parsed map[string]interface{}, filter *StreamFilter) map[string]interface{} {
	content := ExtractDeltaContentFromMap(parsed)
	if content == "" {
		return parsed
	}
	redacted, action, _ := filter.FilterContent(content)
	if action == ActionBlock {
		return nil
	}
	if action == ActionRedact && redacted != content {
		parsed = copyMap(parsed)
		_ = ReplaceDeltaContentMap(parsed, redacted)
	}
	return parsed
}

func applyEntropyFilter(parsed map[string]interface{}, filter *StreamEntropyFilter) map[string]interface{} {
	content := ExtractDeltaContentFromMap(parsed)
	if content == "" {
		return parsed
	}
	redacted, action := filter.FilterContent(content)
	if action == ActionRedact && redacted != content {
		parsed = copyMap(parsed)
		ReplaceDeltaContentMap(parsed, redacted)
	}
	return parsed
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

func (r *SSETransformingReader) extractUsageFromMap(chunk map[string]interface{}) {
	usage, ok := chunk["usage"].(map[string]interface{})
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
	reasoning := ToInt(usage["reasoning_tokens"])
	if allUsageFieldsZero(completion, prompt, total, cacheHit, cacheMiss, cacheCreation, reasoning) {
		return
	}
	dCompletion := clampDelta(completion, r.lastCompletionTokens)
	dPrompt := clampDelta(prompt, r.lastPromptTokens)
	dTotal := clampDelta(total, r.lastTotalTokens)
	dCacheHit := clampDelta(cacheHit, r.lastCacheHitTokens)
	dCacheMiss := clampDelta(cacheMiss, r.lastCacheMissTokens)
	dCacheCreation := clampDelta(cacheCreation, r.lastCacheCreationTokens)
	dReasoning := clampDelta(reasoning, r.lastReasoningTokens)
	if allDeltasZero(dCompletion, dPrompt, dTotal, dCacheHit, dCacheMiss, dCacheCreation, dReasoning) {
		return
	}
	r.lastCompletionTokens = completion
	r.lastPromptTokens = prompt
	r.lastTotalTokens = total
	r.lastCacheHitTokens = cacheHit
	r.lastCacheMissTokens = cacheMiss
	r.lastCacheCreationTokens = cacheCreation
	r.lastReasoningTokens = reasoning
	r.onUsage(dCompletion, dPrompt, dTotal, dCacheHit, dCacheMiss, dCacheCreation, dReasoning)
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

func processToolCallDelta(tcs []interface{}, state *toolCallState) ([]interface{}, bool) {
	keep := make([]interface{}, 0, len(tcs))
	mutated := false

	for _, tcRaw := range tcs {
		tc, ok := tcRaw.(map[string]interface{})
		if !ok {
			continue
		}
		idx := ToInt(tc["index"])

		if state.seenIndices[idx] {
			keep = append(keep, tc)
			continue
		}

		mutated = normalizeToolCallID(tc) || mutated

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

func normalizeToolCallID(tc map[string]interface{}) bool {
	id := tc["id"]
	switch id.(type) {
	case string:
		return false
	case nil:
		idx := ToInt(tc["index"])
		tc["id"] = fmt.Sprintf("call_%d", idx)
		return true
	default:
		idx := ToInt(tc["index"])
		tc["id"] = fmt.Sprintf("call_%d", idx)
		return true
	}
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
	if (tcID == "" || len(tcID) < 6 || tcID[:5] == "call_") && pending.id != "" {
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
