package stream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
)

const (
	SSEScannerInitialBuf = 64 * 1024
	SSEScannerMaxBuf     = 1024 * 1024
)

type ResponseTransformer interface {
	TransformSSEChunk(ctx context.Context, data []byte) ([]byte, error)
}

type UsageCallback func(completionTokens, promptTokens, totalTokens, cacheHitTokens, cacheMissTokens int)

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

	lastCompletionTokens int
	lastPromptTokens     int
	lastTotalTokens      int
	lastCacheHitTokens   int
	lastCacheMissTokens  int

	tcState toolCallState
}

type pendingToolCall struct {
	id   string
	args string
}

type toolCallState struct {
	seenIndices map[int]bool
	pending     map[int]*pendingToolCall
}

func newToolCallState() toolCallState {
	return toolCallState{
		seenIndices: make(map[int]bool),
		pending:     make(map[int]*pendingToolCall),
	}
}

func NewSSETransformingReader(src io.Reader, transformer ResponseTransformer, ctx context.Context) *SSETransformingReader {
	poolBuf := getStreamBuffer()
	reader := &SSETransformingReader{
		src:         src,
		scanner:     bufio.NewScanner(src),
		transformer: transformer,
		ctx:         ctx,
		tcState:     newToolCallState(),
		poolBuf:     poolBuf,
	}
	reader.scanner.Buffer(*poolBuf, SSEScannerMaxBuf)
	return reader
}

func (r *SSETransformingReader) SetOnUsage(cb UsageCallback) {
	r.onUsage = cb
}

func (r *SSETransformingReader) SetStreamFilter(sf *StreamFilter) {
	r.streamFilter = sf
}

func (r *SSETransformingReader) SetStreamEntropyFilter(ef *StreamEntropyFilter) {
	r.streamEntropyFilter = ef
}

func (r *SSETransformingReader) SetOnContent(cb ContentCallback) {
	r.onContent = cb
}

func (r *SSETransformingReader) SetObserver(obs SSEObserver) {
	r.observer = obs
}

// SawDone returns true if the reader has observed a data: [DONE] event.
func (r *SSETransformingReader) SawDone() bool {
	return r.sawDone
}

func (r *SSETransformingReader) Read(p []byte) (int, error) {
	if r.ctx != nil {
		select {
		case <-r.ctx.Done():
			putStreamBuffer(r.poolBuf)
			r.poolBuf = nil
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
		putStreamBuffer(r.poolBuf)
		r.poolBuf = nil
		return 0, r.err
	}

	if !r.scanner.Scan() {
		r.handleScannerDone()
		return r.drainAfterScanDone(p)
	}

	line := r.scanner.Bytes()
	lineCopy := make([]byte, len(line))
	copy(lineCopy, line)
	transformed := r.transformLine(lineCopy)

	if r.streamFilter != nil && r.streamFilter.IsBlocked() {
		r.err = ErrStreamBlocked
		putStreamBuffer(r.poolBuf)
		r.poolBuf = nil
		return 0, r.err
	}

	if !bytes.HasSuffix(transformed, []byte("\n")) {
		transformed = append(transformed, '\n')
	}

	r.buffer = transformed
	r.pos = 0

	return r.Read(p)
}

func (r *SSETransformingReader) transformLine(line []byte) []byte {
	if len(line) == 0 {
		return line
	}

	if !bytes.HasPrefix(line, []byte("data: ")) {
		return r.transformNonSSELine(line)
	}

	return r.transformSSEData(line)
}

// handleScannerDone is called when the scanner has no more input.
// It sets r.err and optionally injects error+[DONE] into r.buffer.
func (r *SSETransformingReader) handleScannerDone() {
	switch r.scanner.Err() {
	case nil:
		if !r.sawDone {
			r.injectErrorBuffer("upstream stream ended without [DONE]")
		}
		r.err = io.EOF
	case bufio.ErrTooLong:
		r.injectErrorBuffer("upstream SSE line exceeded maximum scanner buffer")
		r.err = r.scanner.Err()
	default:
		r.err = r.scanner.Err()
	}
}

// injectErrorBuffer creates a gateway_error SSE event + [DONE] and places it
// in r.buffer so the client receives the error before EOF.
func (r *SSETransformingReader) injectErrorBuffer(message string) {
	errPayload, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "gateway_error",
		},
	})
	r.buffer = append(append([]byte("data: "), errPayload...), []byte("\n\ndata: [DONE]\n\n")...)
	r.pos = 0
	if r.observer != nil {
		var errMap map[string]any
		_ = json.Unmarshal(errPayload, &errMap)
		r.observer.OnSSEEvent(SSEEvent{Type: "error", Data: errMap})
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
	putStreamBuffer(r.poolBuf)
	r.poolBuf = nil
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
		return r.handleNoTransformer(parsed, data, origData, line)
	}

	return r.handleWithTransformer(parsed, data, origData, line)
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
		r.notifySSEObserver(line, parsed, "")
		return line
	}

	transformed = r.applyToolCallNormalization(transformed, &r.tcState)

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
	if err != nil || bytes.Equal(transformed, trimmed) {
		return line
	}
	return transformed
}

func (r *SSETransformingReader) extractUsageFromMap(chunk map[string]interface{}) {
	usage, ok := chunk["usage"].(map[string]interface{})
	if !ok {
		return
	}
	completion := ToInt(usage["completion_tokens"])
	prompt := ToInt(usage["prompt_tokens"])
	total := ToInt(usage["total_tokens"])
	cacheHit := ToInt(usage["prompt_cache_hit_tokens"])
	cacheMiss := ToInt(usage["prompt_cache_miss_tokens"])
	if completion == 0 && prompt == 0 && total == 0 && cacheHit == 0 && cacheMiss == 0 {
		return
	}
	dCompletion := completion - r.lastCompletionTokens
	dPrompt := prompt - r.lastPromptTokens
	dTotal := total - r.lastTotalTokens
	dCacheHit := cacheHit - r.lastCacheHitTokens
	dCacheMiss := cacheMiss - r.lastCacheMissTokens
	if dCompletion <= 0 && dPrompt <= 0 && dTotal <= 0 && dCacheHit <= 0 && dCacheMiss <= 0 {
		return
	}
	r.lastCompletionTokens = completion
	r.lastPromptTokens = prompt
	r.lastTotalTokens = total
	r.lastCacheHitTokens = cacheHit
	r.lastCacheMissTokens = cacheMiss
	r.onUsage(dCompletion, dPrompt, dTotal, dCacheHit, dCacheMiss)
}

// ToInt converts an interface{} value to int, handling float64 and int types.
// Returns 0 for unsupported types.
func ToInt(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

// normalizeToolCalls processes tool_calls deltas in a streaming response chunk.
// It normalizes missing tool_call IDs, buffers args chunks that arrive before names,
// and merges pending data when names arrive. Returns true if the chunk was mutated.
func normalizeToolCalls(chunk map[string]interface{}, state *toolCallState) bool {
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
