package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

const (
	sseScannerInitialBuf = 64 * 1024
	sseScannerMaxBuf     = 1024 * 1024
)

// ResponseTransformer defines the interface for provider-specific response normalization.
type ResponseTransformer interface {
	TransformSSEChunk(data []byte) ([]byte, error)
}

// GeminiTransformer fixes Gemini's OpenAI-compatible API response format.
// Gemini streaming responses may omit the 'index' field inside each tool_calls
// entry. This transformer adds the missing index to comply with the OpenAI spec.
// When onExtraContent is set, tool_call entries containing 'extra_content'
// (e.g. thought_signature for Gemini 3) are cached for re-injection on
// subsequent requests.
type GeminiTransformer struct {
	onExtraContent func(toolCallID string, extraContent interface{})
}

func (t *GeminiTransformer) TransformSSEChunk(data []byte) ([]byte, error) {
	if len(data) == 0 || !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return data, nil
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return data, nil
	}

	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if delta, ok := choice["delta"].(map[string]interface{}); ok {
				if toolCalls, ok := delta["tool_calls"].([]interface{}); ok {
					for i, tc := range toolCalls {
						if tcMap, ok := tc.(map[string]interface{}); ok {
							if _, exists := tcMap["index"]; !exists {
								tcMap["index"] = i
							}
							if t.onExtraContent != nil {
								if tcID, _ := tcMap["id"].(string); tcID != "" {
									if extra, hasExtra := tcMap["extra_content"]; hasExtra {
										t.onExtraContent(tcID, extra)
									}
								}
							}
						}
					}
				}
			}
		}
	}

	transformed, err := json.Marshal(chunk)
	if err != nil {
		return data, fmt.Errorf("failed to marshal transformed chunk: %v", err)
	}

	return transformed, nil
}

// UsageCallback is called when a usage field is found in an SSE chunk.
type UsageCallback func(completionTokens, promptTokens, totalTokens int)

// SSETransformingReader wraps an io.Reader and applies response transformations
// to Server-Sent Events (SSE) stream lines.
type SSETransformingReader struct {
	src          io.Reader
	scanner      *bufio.Scanner
	transformer  ResponseTransformer
	onUsage      UsageCallback
	streamFilter *StreamFilter
	buffer       []byte
	pos          int
	err          error
	usageFired   bool
}

// NewSSETransformingReader creates a new transforming reader for SSE streams.
func NewSSETransformingReader(src io.Reader, transformer ResponseTransformer) *SSETransformingReader {
	reader := &SSETransformingReader{
		src:         src,
		scanner:     bufio.NewScanner(src),
		transformer: transformer,
	}
	// Increase scanner buffer size to handle large JSON chunks
	reader.scanner.Buffer(make([]byte, 0, sseScannerInitialBuf), sseScannerMaxBuf)
	return reader
}

// SetOnUsage sets a callback invoked when the stream contains a usage field.
func (r *SSETransformingReader) SetOnUsage(cb UsageCallback) {
	r.onUsage = cb
}

func (r *SSETransformingReader) SetStreamFilter(sf *StreamFilter) {
	r.streamFilter = sf
}

// Read implements io.Reader interface, transforming SSE lines as they're read.
func (r *SSETransformingReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}

	// If we have buffered data from previous transformation, return it first
	if r.pos < len(r.buffer) {
		n := copy(p, r.buffer[r.pos:])
		r.pos += n
		if r.pos >= len(r.buffer) {
			r.buffer = nil
			r.pos = 0
		}
		return n, nil
	}

	// Read next line from source
	if !r.scanner.Scan() {
		r.err = r.scanner.Err()
		if r.err == nil {
			r.err = io.EOF
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

	// Buffer the transformed line for next Read call
	r.buffer = transformed
	r.pos = 0

	// Copy to output buffer
	return r.Read(p)
}

// transformLine processes a single SSE line, applying transformations if needed.
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
			if content := extractDeltaContentFromMap(parsed); content != "" {
				redacted, action, _ := r.streamFilter.FilterContent(content)
				if action == ActionBlock {
					return line
				}
				if action == ActionRedact && redacted != content {
					data = replaceDeltaContentMap(parsed, redacted)
				}
			}
		}

		if r.onUsage != nil && !r.usageFired && parsed != nil {
			r.extractUsageFromMap(parsed)
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

func (g *NenyaGateway) getResponseTransformer(providerName string) ResponseTransformer {
	if g.isGeminiProvider(providerName) {
		return &GeminiTransformer{
			onExtraContent: func(toolCallID string, extraContent interface{}) {
				g.thoughtSigCache.Store(toolCallID, extraContent)
			},
		}
	}

	return nil
}

func (r *SSETransformingReader) extractUsageFromMap(chunk map[string]interface{}) {
	usage, ok := chunk["usage"].(map[string]interface{})
	if !ok {
		return
	}
	completion := toInt(usage["completion_tokens"])
	prompt := toInt(usage["prompt_tokens"])
	total := toInt(usage["total_tokens"])
	if completion == 0 && prompt == 0 && total == 0 {
		return
	}
	r.usageFired = true
	r.onUsage(completion, prompt, total)
}

func toInt(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}
