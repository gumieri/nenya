package stream

import (
	"bufio"
	"bytes"
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
	TransformSSEChunk(data []byte) ([]byte, error)
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

func NewSSETransformingReader(src io.Reader, transformer ResponseTransformer) *SSETransformingReader {
	reader := &SSETransformingReader{
		src:         src,
		scanner:     bufio.NewScanner(src),
		transformer: transformer,
		tcState:     newToolCallState(),
	}
	reader.scanner.Buffer(make([]byte, 0, SSEScannerInitialBuf), SSEScannerMaxBuf)
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

func (r *SSETransformingReader) Read(p []byte) (int, error) {
	// Drain pending buffer before returning any error so that an error
	// event we injected (e.g. ErrTooLong) reaches the client.
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
		return 0, r.err
	}

	if !r.scanner.Scan() {
		switch r.scanner.Err() {
		case nil:
			r.err = io.EOF
		case bufio.ErrTooLong:
			errPayload, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"message": "upstream SSE line exceeded maximum scanner buffer",
					"type":    "gateway_error",
				},
			})
			r.buffer = append(append([]byte("data: "), errPayload...), []byte("\n\ndata: [DONE]\n\n")...)
			r.pos = 0
			r.err = r.scanner.Err()
			if r.observer != nil {
				var errMap map[string]any
				_ = json.Unmarshal(errPayload, &errMap)
				r.observer.OnSSEEvent(SSEEvent{
					Type: "error",
					Data: errMap,
				})
			}
		default:
			r.err = r.scanner.Err()
		}
		if r.observer != nil && !r.closed {
			r.closed = true
			r.observer.OnStreamClose(r.err)
		}
		return 0, r.err
	}

	line := r.scanner.Bytes()
	lineCopy := make([]byte, len(line))
	copy(lineCopy, line)
	transformed := r.transformLine(lineCopy)

	if r.streamFilter != nil && r.streamFilter.IsBlocked() {
		r.err = ErrStreamBlocked
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

	if bytes.HasPrefix(line, []byte("data: ")) {
		origData := bytes.TrimPrefix(line, []byte("data: "))

		if len(origData) == 0 || bytes.Equal(origData, []byte("[DONE]")) {
			if r.observer != nil {
				r.observer.OnSSEEvent(SSEEvent{Type: "done", Raw: line})
			}
			return line
		}

		data := origData

		var parsed map[string]interface{}
		if bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
			if err := json.Unmarshal(data, &parsed); err != nil {
				parsed = nil
			}
		}

		if r.streamFilter != nil && !r.streamFilter.IsBlocked() && parsed != nil {
			if content := ExtractDeltaContentFromMap(parsed); content != "" {
				redacted, action, _ := r.streamFilter.FilterContent(content)
				if action == ActionBlock {
					return line
				}
				if action == ActionRedact && redacted != content {
					data = ReplaceDeltaContentMap(parsed, redacted)
				}
			}
		}

		if r.streamEntropyFilter != nil && parsed != nil {
			if content := ExtractDeltaContentFromMap(parsed); content != "" {
				redacted, action := r.streamEntropyFilter.FilterContent(content)
				if action == ActionRedact && redacted != content {
					data = ReplaceDeltaContentMap(parsed, redacted)
				}
			}
		}

		if r.onUsage != nil && parsed != nil {
			r.extractUsageFromMap(parsed)
		}

		if r.onContent != nil && parsed != nil {
			if content := ExtractDeltaContentFromMap(parsed); content != "" {
				r.onContent(content)
			}
		}

		if r.transformer == nil {
			if parsed != nil {
				if normalizeToolCalls(parsed, &r.tcState) {
					if out, err := json.Marshal(parsed); err == nil {
						data = out
					}
				}
			}
			if bytes.Equal(data, origData) {
				if r.observer != nil {
					r.observer.OnSSEEvent(SSEEvent{Raw: line, Data: parsed})
				}
				return line
			}
			finalLine := append([]byte("data: "), data...)
			if r.observer != nil {
				r.observer.OnSSEEvent(SSEEvent{Raw: finalLine, Data: parsed})
			}
			return finalLine
		}

		transformed, err := r.transformer.TransformSSEChunk(data)
		if err != nil {
			if r.observer != nil {
				r.observer.OnSSEEvent(SSEEvent{Raw: line, Data: parsed})
			}
			return line
		}

		if len(transformed) > 0 && transformed[0] == '{' {
			var transformedParsed map[string]interface{}
			if json.Unmarshal(transformed, &transformedParsed) == nil {
				if normalizeToolCalls(transformedParsed, &r.tcState) {
					if out, err := json.Marshal(transformedParsed); err == nil {
						transformed = out
					}
				}
			}
		}

		if bytes.Equal(transformed, origData) && bytes.Equal(data, origData) {
			if r.observer != nil {
				r.observer.OnSSEEvent(SSEEvent{Raw: line, Data: parsed})
			}
			return line
		}

		finalLine := append([]byte("data: "), transformed...)
		if r.observer != nil {
			r.observer.OnSSEEvent(SSEEvent{Raw: finalLine, Data: parsed})
		}
		return finalLine
	}

	trimmed := bytes.TrimSpace(line)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		if r.transformer == nil {
			return line
		}
		transformed, err := r.transformer.TransformSSEChunk(trimmed)
		if err != nil || bytes.Equal(transformed, trimmed) {
			return line
		}
		return transformed
	}

	return line
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
		keep := make([]interface{}, 0, len(tcs))
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

			id := tc["id"]
			switch id.(type) {
			case string:
			case nil:
				tc["id"] = fmt.Sprintf("call_%d", idx)
				mutated = true
			default:
				tc["id"] = fmt.Sprintf("call_%d", idx)
				mutated = true
			}

			fn, hasFn := tc["function"]
			tcID, _ := tc["id"].(string)
			fnNameStr := extractToolCallName(fn, hasFn, &mutated, tcID)
			fnArgsStr := extractToolCallArgs(fn, hasFn)

			if fnNameStr != "" {
				if p, ok := state.pending[idx]; ok {
					if (tcID == "" || len(tcID) < 6 || tcID[:5] == "call_") && p.id != "" {
						tc["id"] = p.id
					}
					if fnArgsStr == "" || fnArgsStr == "{}" {
						if p.args != "" {
							if fn, ok := tc["function"].(map[string]interface{}); ok {
								fn["arguments"] = p.args
							}
						}
					} else if p.args != "" {
						if fn, ok := tc["function"].(map[string]interface{}); ok {
							fn["arguments"] = p.args + fnArgsStr
						}
					}
					delete(state.pending, idx)
					mutated = true
					slog.Debug("merged pending tool_call data on name arrival",
						"index", idx,
						"pending_args_len", len(p.args),
						"tool_call_id", tcID,
					)
				}
				state.seenIndices[idx] = true
				keep = append(keep, tc)
				continue
			}

			if fnArgsStr != "" && fnArgsStr != "{}" {
				state.pending[idx] = &pendingToolCall{
					id:   tcID,
					args: fnArgsStr,
				}
				mutated = true
				slog.Debug("buffered tool_call entry missing name, waiting for name chunk",
					"index", idx,
					"buffered_args_len", len(fnArgsStr),
					"tool_call_id", tcID,
				)
			}
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
