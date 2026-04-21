package stream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

const (
	SSEScannerInitialBuf = 64 * 1024
	SSEScannerMaxBuf     = 1024 * 1024
)

type ResponseTransformer interface {
	TransformSSEChunk(data []byte) ([]byte, error)
}

type UsageCallback func(completionTokens, promptTokens, totalTokens int)

type ContentCallback func(content string)

type SSETransformingReader struct {
	src                 io.Reader
	scanner             *bufio.Scanner
	transformer         ResponseTransformer
	onUsage             UsageCallback
	onContent           ContentCallback
	streamFilter        *StreamFilter
	streamEntropyFilter *StreamEntropyFilter
	buffer              []byte
	pos                 int
	err                 error

	// Track last seen token counts to deliver deltas when providers
	// emit usage in multiple chunks.
	lastCompletionTokens int
	lastPromptTokens     int
	lastTotalTokens      int
}

func NewSSETransformingReader(src io.Reader, transformer ResponseTransformer) *SSETransformingReader {
	reader := &SSETransformingReader{
		src:         src,
		scanner:     bufio.NewScanner(src),
		transformer: transformer,
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
		scanErr := r.scanner.Err()
		if scanErr == nil {
			r.err = io.EOF
			return 0, r.err
		}
		if scanErr == bufio.ErrTooLong {
			// Surface a parseable SSE error chunk so the client knows why
			// the stream ended rather than seeing a silent EOF.
			errPayload, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"message": "upstream SSE line exceeded maximum scanner buffer",
					"type":    "gateway_error",
				},
			})
			r.buffer = append(append([]byte("data: "), errPayload...), []byte("\n\ndata: [DONE]\n\n")...)
			r.pos = 0
			r.err = scanErr
			return r.Read(p)
		}
		r.err = scanErr
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

		if parsed != nil {
			if normalizeToolCalls(parsed) {
				if out, err := json.Marshal(parsed); err == nil {
					data = out
				}
			}
		}

		if r.onContent != nil && parsed != nil {
			if content := ExtractDeltaContentFromMap(parsed); content != "" {
				r.onContent(content)
			}
		}

		if r.transformer == nil {
			if bytes.Equal(data, origData) {
				return line
			}
			return append([]byte("data: "), data...)
		}

		transformed, err := r.transformer.TransformSSEChunk(data)
		if err != nil {
			return line
		}

		if bytes.Equal(transformed, origData) && bytes.Equal(data, origData) {
			return line
		}

		return append([]byte("data: "), transformed...)
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
	if completion == 0 && prompt == 0 && total == 0 {
		return
	}
	// Compute deltas so providers that emit cumulative usage in multiple
	// chunks don't cause double-counting in additive stats trackers.
	dCompletion := completion - r.lastCompletionTokens
	dPrompt := prompt - r.lastPromptTokens
	dTotal := total - r.lastTotalTokens
	if dCompletion <= 0 && dPrompt <= 0 && dTotal <= 0 {
		return
	}
	r.lastCompletionTokens = completion
	r.lastPromptTokens = prompt
	r.lastTotalTokens = total
	r.onUsage(dCompletion, dPrompt, dTotal)
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

func normalizeToolCalls(chunk map[string]interface{}) bool {
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
			if !hasFn || fn == nil {
				continue
			}
			fnMap, ok := fn.(map[string]interface{})
			if !ok {
				continue
			}
			fnName, hasFnName := fnMap["name"]
			fnArgs, hasFnArgs := fnMap["arguments"]
			if !hasFnName || fnName == nil {
				if !hasFnArgs || fnArgs == nil || fnArgs == "" {
					continue
				}
			}
			keep = append(keep, tc)
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
