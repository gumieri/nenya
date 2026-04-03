package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// ResponseTransformer defines the interface for provider-specific response normalization.
type ResponseTransformer interface {
	TransformSSEChunk(data []byte) ([]byte, error)
}

// GeminiTransformer fixes Gemini's OpenAI-compatible API response format.
// Gemini responses are missing required 'index' field in tool_calls and include
// non-standard 'extra_content' field.
type GeminiTransformer struct{}

func (t *GeminiTransformer) TransformSSEChunk(data []byte) ([]byte, error) {
	// Empty data or non-JSON (e.g., "[DONE]") passes through unchanged
	if len(data) == 0 || !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return data, nil
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal(data, &chunk); err != nil {
		// Not valid JSON, pass through (could be malformed but we don't want to break streaming)
		return data, nil
	}

	// Fix tool_calls format in choices[0].delta.tool_calls
	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if delta, ok := choice["delta"].(map[string]interface{}); ok {
				if toolCalls, ok := delta["tool_calls"].([]interface{}); ok {
					for i, tc := range toolCalls {
						if tcMap, ok := tc.(map[string]interface{}); ok {
							// Add missing index field required by OpenAI spec
							if _, exists := tcMap["index"]; !exists {
								tcMap["index"] = i
							}
							// Remove Gemini-specific extra_content field
							delete(tcMap, "extra_content")
						}
					}
				}
			}
		}
	}

	// Re-marshal the transformed chunk
	transformed, err := json.Marshal(chunk)
	if err != nil {
		// If marshaling fails, return original data to avoid breaking the stream
		return data, fmt.Errorf("failed to marshal transformed chunk: %v", err)
	}

	return transformed, nil
}

// UsageCallback is called when a usage field is found in an SSE chunk.
type UsageCallback func(completionTokens, promptTokens, totalTokens int)

// SSETransformingReader wraps an io.Reader and applies response transformations
// to Server-Sent Events (SSE) stream lines.
type SSETransformingReader struct {
	src         io.Reader
	scanner     *bufio.Scanner
	transformer ResponseTransformer
	onUsage     UsageCallback
	buffer      []byte
	pos         int
	err         error
	usageFired  bool
}

// NewSSETransformingReader creates a new transforming reader for SSE streams.
func NewSSETransformingReader(src io.Reader, transformer ResponseTransformer) *SSETransformingReader {
	reader := &SSETransformingReader{
		src:         src,
		scanner:     bufio.NewScanner(src),
		transformer: transformer,
	}
	// Increase scanner buffer size to handle large JSON chunks
	reader.scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 1MB max line
	return reader
}

// SetOnUsage sets a callback invoked when the stream contains a usage field.
func (r *SSETransformingReader) SetOnUsage(cb UsageCallback) {
	r.onUsage = cb
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
	transformed := r.transformLine(line)

	// Ensure newline is preserved
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
	// Skip empty lines
	if len(line) == 0 {
		return line
	}

	// Handle SSE data lines: "data: {json}"
	if bytes.HasPrefix(line, []byte("data: ")) {
		data := bytes.TrimPrefix(line, []byte("data: "))

		// Skip "[DONE]" and other non-JSON data messages
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			return line
		}

		// Extract usage stats before transformation
		if r.onUsage != nil && !r.usageFired {
			r.extractUsage(data)
		}

		if r.transformer == nil {
			return line
		}

		transformed, err := r.transformer.TransformSSEChunk(data)
		if err != nil {
			return line
		}

		// If transformer returned same data, no change needed
		if bytes.Equal(transformed, data) {
			return line
		}

		// Reconstruct SSE line with transformed data
		return append([]byte("data: "), transformed...)
	}

	// For non-SSE lines or raw JSON streaming (no "data: " prefix),
	// check if it looks like JSON and transform it
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

	// Pass through all other lines unchanged (comments, event types, etc.)
	return line
}

var geminiTransformer ResponseTransformer = &GeminiTransformer{}

func (g *NenyaGateway) getResponseTransformer(providerName string) ResponseTransformer {
	if g.isGeminiProvider(providerName) {
		return geminiTransformer
	}

	return nil
}

func (r *SSETransformingReader) extractUsage(data []byte) {
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return
	}
	var chunk map[string]interface{}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return
	}
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
