package stream

import (
	"bufio"
	"bytes"
	"encoding/json"
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
	usageFired          bool
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
	if r.err != nil {
		return 0, r.err
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

		if r.onUsage != nil && !r.usageFired && parsed != nil {
			r.extractUsageFromMap(parsed)
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
	r.usageFired = true
	r.onUsage(completion, prompt, total)
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
