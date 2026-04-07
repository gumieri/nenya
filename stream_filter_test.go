package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestStreamFilter_Pass(t *testing.T) {
	sf := NewStreamFilter(nil, nil, "[REDACTED]", 4096)

	out, action, reason := sf.FilterContent("Hello world")
	if action != ActionPass {
		t.Errorf("expected ActionPass, got %d", action)
	}
	if reason != "" {
		t.Errorf("expected empty reason, got %q", reason)
	}
	if out != "Hello world" {
		t.Errorf("expected unchanged output, got %q", out)
	}
}

func TestStreamFilter_RedactSecret(t *testing.T) {
	secretRe := regexp.MustCompile(`(?i)AKIA[0-9A-Z]{16}`)
	sf := NewStreamFilter([]*regexp.Regexp{secretRe}, nil, "[REDACTED]", 4096)

	content := "The key is AKIAIOSFODNN7EXAMPLE and it should be hidden"
	out, action, reason := sf.FilterContent(content)

	if action != ActionRedact {
		t.Fatalf("expected ActionRedact, got %d", action)
	}
	if reason != "" {
		t.Errorf("expected empty reason for redact, got %q", reason)
	}
	if strings.Contains(out, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("secret not redacted: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected redaction label: %q", out)
	}
}

func TestStreamFilter_BlockDestructiveCommand(t *testing.T) {
	blockRe := regexp.MustCompile(`(?i)\brm\s+-[a-zA-Z]*[rR][a-zA-Z]*\s+.*(/|\*)`)
	sf := NewStreamFilter(nil, []*regexp.Regexp{blockRe}, "[REDACTED]", 4096)

	content := "Now running rm -rf / to clean up"
	out, action, reason := sf.FilterContent(content)

	if action != ActionBlock {
		t.Fatalf("expected ActionBlock, got %d", action)
	}
	if reason == "" {
		t.Error("expected non-empty reason for block")
	}
	if !sf.IsBlocked() {
		t.Error("expected IsBlocked() to be true")
	}
	if out != content {
		t.Errorf("block should not modify content, got %q", out)
	}
}

func TestStreamFilter_BlockTakesPrecedence(t *testing.T) {
	secretRe := regexp.MustCompile(`(?i)AKIA[0-9A-Z]{16}`)
	blockRe := regexp.MustCompile(`(?i)\brm\s+-[a-zA-Z]*[rR][a-zA-Z]*\s+.*(/|\*)`)
	sf := NewStreamFilter([]*regexp.Regexp{secretRe}, []*regexp.Regexp{blockRe}, "[REDACTED]", 4096)

	content := "rm -rf / and key AKIAIOSFODNN7EXAMPLE"
	_, action, _ := sf.FilterContent(content)

	if action != ActionBlock {
		t.Fatalf("expected ActionBlock (precedence), got %d", action)
	}
}

func TestStreamFilter_OnceBlockedStaysBlocked(t *testing.T) {
	blockRe := regexp.MustCompile(`(?i)\brm\s+-rf\b`)
	sf := NewStreamFilter(nil, []*regexp.Regexp{blockRe}, "[REDACTED]", 4096)

	sf.FilterContent("rm -rf /")
	if !sf.IsBlocked() {
		t.Fatal("expected blocked after first match")
	}

	out, action, _ := sf.FilterContent("Hello safe content")
	if action != ActionBlock {
		t.Errorf("expected ActionBlock on subsequent call, got %d", action)
	}
	if out != "Hello safe content" {
		t.Errorf("content should be unchanged on subsequent block, got %q", out)
	}
}

func TestStreamFilter_EmptyContent(t *testing.T) {
	secretRe := regexp.MustCompile(`(?i)AKIA[0-9A-Z]{16}`)
	sf := NewStreamFilter([]*regexp.Regexp{secretRe}, nil, "[REDACTED]", 4096)

	out, action, _ := sf.FilterContent("")
	if action != ActionPass {
		t.Errorf("expected ActionPass for empty, got %d", action)
	}
	if out != "" {
		t.Errorf("expected empty output, got %q", out)
	}
}

func TestStreamFilter_NilPatterns(t *testing.T) {
	sf := NewStreamFilter(nil, nil, "[REDACTED]", 4096)

	out, action, _ := sf.FilterContent("AKIAIOSFODNN7EXAMPLE rm -rf /")
	if action != ActionPass {
		t.Errorf("expected ActionPass with nil patterns, got %d", action)
	}
	if out != "AKIAIOSFODNN7EXAMPLE rm -rf /" {
		t.Errorf("expected unchanged, got %q", out)
	}
}

func TestStreamFilter_SlidingWindowCrossChunkSecret(t *testing.T) {
	secretRe := regexp.MustCompile(`(?i)AKIA[0-9A-Z]{16}`)
	sf := NewStreamFilter([]*regexp.Regexp{secretRe}, nil, "[REDACTED]", 100)

	sf.FilterContent("The key is AKIAIOSFODNN")

	out, action, _ := sf.FilterContent("7EXAMPLE and should be hidden")
	if action != ActionRedact {
		t.Fatalf("expected ActionRedact from window cross-chunk detection, got %d", action)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected redaction in window match, got %q", out)
	}
}

func TestStreamFilter_SlidingWindowCrossChunkBlock(t *testing.T) {
	blockRe := regexp.MustCompile(`(?i)\brm\s+-rf\b`)
	sf := NewStreamFilter(nil, []*regexp.Regexp{blockRe}, "[REDACTED]", 100)

	sf.FilterContent("I will now run rm -")
	_, action, _ := sf.FilterContent("rf /var/log")

	if action != ActionBlock {
		t.Fatalf("expected ActionBlock from window cross-chunk detection, got %d", action)
	}
	if !sf.IsBlocked() {
		t.Error("expected IsBlocked() after cross-chunk block")
	}
}

func TestStreamFilter_SlidingWindowEviction(t *testing.T) {
	blockRe := regexp.MustCompile(`(?i)\bterraform\s+destroy\b`)
	sf := NewStreamFilter(nil, []*regexp.Regexp{blockRe}, "[REDACTED]", 20)

	sf.FilterContent("padding text to fill the window buffer with lots of chars")
	_, action, _ := sf.FilterContent("terraform destroy now")

	if action != ActionBlock {
		t.Fatalf("expected ActionBlock, window should still catch it, got %d", action)
	}
}

func TestStreamFilter_WindowTooSmallForCrossChunk(t *testing.T) {
	secretRe := regexp.MustCompile(`(?i)AKIA[0-9A-Z]{16}`)
	sf := NewStreamFilter([]*regexp.Regexp{secretRe}, nil, "[REDACTED]", 5)

	sf.FilterContent("AKIA")
	_, action, _ := sf.FilterContent("IOSFODNN7EXAMPLE")

	if action != ActionPass {
		t.Errorf("expected ActionPass (window too small to hold both chunks), got %d", action)
	}
}

func TestStreamFilter_WindowLen(t *testing.T) {
	sf := NewStreamFilter(nil, nil, "[REDACTED]", 10)

	if sf.WindowLen() != 0 {
		t.Errorf("expected 0, got %d", sf.WindowLen())
	}

	sf.FilterContent("hello")

	if sf.WindowLen() != 5 {
		t.Errorf("expected 5, got %d", sf.WindowLen())
	}
}

func TestStreamFilter_WindowContent(t *testing.T) {
	sf := NewStreamFilter(nil, nil, "[REDACTED]", 100)

	sf.FilterContent("abc")
	sf.FilterContent("def")

	if sf.WindowContent() != "abcdef" {
		t.Errorf("expected 'abcdef', got %q", sf.WindowContent())
	}
}

func TestStreamFilter_DefaultWindowSize(t *testing.T) {
	sf := NewStreamFilter(nil, nil, "[REDACTED]", 0)

	if sf.windowSize != 4096 {
		t.Errorf("expected default 4096, got %d", sf.windowSize)
	}
}

func TestExtractDeltaContent(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"non-json", "not json", ""},
		{"no choices", `{"model":"test"}`, ""},
		{"empty choices", `{"choices":[]}`, ""},
		{"no delta", `{"choices":[{"finish_reason":"stop"}]}`, ""},
		{"with content", `{"choices":[{"delta":{"content":"hello"}}]}`, "hello"},
		{"with role and content", `{"choices":[{"delta":{"role":"assistant","content":"world"}}]}`, "world"},
		{"tool_calls no content", `{"choices":[{"delta":{"tool_calls":[{"id":"1"}]}}]}`, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractDeltaContent([]byte(tt.input))
			if got != tt.want {
				t.Errorf("extractDeltaContent() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReplaceDeltaContent(t *testing.T) {
	input := `{"choices":[{"delta":{"content":"hello world"}}]}`

	result := replaceDeltaContent([]byte(input), "redacted")

	if !strings.Contains(string(result), "redacted") {
		t.Errorf("expected redacted content in output, got %s", result)
	}
	if strings.Contains(string(result), "hello world") {
		t.Error("original content should be replaced")
	}
}

func TestReplaceDeltaContent_InvalidJSON(t *testing.T) {
	input := "not json"

	result := replaceDeltaContent([]byte(input), "redacted")

	if string(result) != "not json" {
		t.Errorf("expected unchanged for invalid JSON, got %s", result)
	}
}

func TestSSETransformingReader_StreamFilterBlock(t *testing.T) {
	blockRe := regexp.MustCompile(`(?i)\brm\s+-rf\b`)
	sf := NewStreamFilter(nil, []*regexp.Regexp{blockRe}, "[REDACTED]", 4096)

	sseStream := `data: {"choices":[{"delta":{"content":"safe text"}}]}
data: {"choices":[{"delta":{"content":"about to rm -rf /"}}]}
data: [DONE]
`

	transformingReader := NewSSETransformingReader(strings.NewReader(sseStream), nil)
	transformingReader.SetStreamFilter(sf)

	var buf strings.Builder
	_, err := io.Copy(&buf, transformingReader)

	if !errors.Is(err, ErrStreamBlocked) {
		t.Fatalf("expected ErrStreamBlocked, got %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "safe text") {
		t.Error("expected first chunk to pass through")
	}
}

func TestSSETransformingReader_StreamFilterRedact(t *testing.T) {
	secretRe := regexp.MustCompile(`(?i)AKIA[0-9A-Z]{16}`)
	sf := NewStreamFilter([]*regexp.Regexp{secretRe}, nil, "[REDACTED]", 4096)

	sseStream := `data: {"choices":[{"delta":{"content":"key is AKIAIOSFODNN7EXAMPLE"}}]}
data: [DONE]
`

	transformingReader := NewSSETransformingReader(strings.NewReader(sseStream), nil)
	transformingReader.SetStreamFilter(sf)

	var buf strings.Builder
	_, err := io.Copy(&buf, transformingReader)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if strings.Contains(output, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("secret not redacted in stream output: %s", output)
	}
	if !strings.Contains(output, "[REDACTED]") {
		t.Errorf("expected redaction label in output: %s", output)
	}
}

func TestSSETransformingReader_StreamFilterNil(t *testing.T) {
	sseStream := `data: {"choices":[{"delta":{"content":"rm -rf / and AKIAIOSFODNN7EXAMPLE"}}]}
data: [DONE]
`

	transformingReader := NewSSETransformingReader(strings.NewReader(sseStream), nil)

	var buf strings.Builder
	_, err := io.Copy(&buf, transformingReader)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "rm -rf /") {
		t.Error("content should pass through with no filter")
	}
}

func TestStreamFilter_DropTable(t *testing.T) {
	blockRe := regexp.MustCompile(`(?i)\b(DROP|TRUNCATE)\s+(TABLE|DATABASE|SCHEMA)\b`)
	sf := NewStreamFilter(nil, []*regexp.Regexp{blockRe}, "[REDACTED]", 4096)

	_, action, _ := sf.FilterContent("DROP TABLE users;")
	if action != ActionBlock {
		t.Errorf("expected ActionBlock for DROP TABLE, got %d", action)
	}

	_, action, _ = sf.FilterContent("truncate table sessions")
	if action != ActionBlock {
		t.Errorf("expected ActionBlock for truncate table, got %d", action)
	}
}

func TestStreamFilter_TerraformDestroy(t *testing.T) {
	blockRe := regexp.MustCompile(`(?i)\bterraform\s+destroy\b`)
	sf := NewStreamFilter(nil, []*regexp.Regexp{blockRe}, "[REDACTED]", 4096)

	_, action, _ := sf.FilterContent("terraform destroy -auto-approve")
	if action != ActionBlock {
		t.Errorf("expected ActionBlock, got %d", action)
	}
}

func TestStreamFilter_KubectlDeleteNs(t *testing.T) {
	blockRe := regexp.MustCompile(`(?i)\bkubectl\s+delete\s+(namespace|ns|pv|pvc|crd)\b`)
	sf := NewStreamFilter(nil, []*regexp.Regexp{blockRe}, "[REDACTED]", 4096)

	_, action, _ := sf.FilterContent("kubectl delete namespace production")
	if action != ActionBlock {
		t.Errorf("expected ActionBlock, got %d", action)
	}

	_, action, _ = sf.FilterContent("kubectl delete ns staging")
	if action != ActionBlock {
		t.Errorf("expected ActionBlock, got %d", action)
	}
}

func TestStreamFilter_PrivateKeyRedaction(t *testing.T) {
	secretRe := regexp.MustCompile(`(?i)-----BEGIN\s+(RSA\s+)?(DSA\s+)?(EC\s+)?PRIVATE\s+KEY\s*-----`)
	sf := NewStreamFilter([]*regexp.Regexp{secretRe}, nil, "[REDACTED]", 4096)

	tests := []struct {
		name    string
		content string
	}{
		{"RSA", "-----BEGIN RSA PRIVATE KEY-----"},
		{"EC", "-----BEGIN EC PRIVATE KEY-----"},
		{"DSA", "-----BEGIN DSA PRIVATE KEY-----"},
		{"PKCS8", "-----BEGIN PRIVATE KEY-----"},
		{"RSA with trailing dash", "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAK"},
		{"inline", "the key is -----BEGIN EC PRIVATE KEY----- and starts here"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, action, _ := sf.FilterContent(tt.content)
			if action != ActionRedact {
				t.Fatalf("expected ActionRedact, got %d for %q", action, tt.content)
			}
			if strings.Contains(out, "PRIVATE KEY") {
				t.Errorf("private key header not redacted: %q", out)
			}
			if !strings.Contains(out, "[REDACTED]") {
				t.Errorf("expected redaction label in output: %q", out)
			}
		})
	}
}

func TestStreamFilter_DefaultPrivateKeyPatternCompiles(t *testing.T) {
	pattern := `(?i)-----BEGIN\s+(RSA\s+)?(DSA\s+)?(EC\s+)?PRIVATE\s+KEY\s*-----`
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("default private key pattern failed to compile: %v", err)
	}

	tests := []string{
		"-----BEGIN RSA PRIVATE KEY-----",
		"-----BEGIN EC PRIVATE KEY-----",
		"-----BEGIN DSA PRIVATE KEY-----",
		"-----BEGIN PRIVATE KEY-----",
	}
	for _, tc := range tests {
		if !re.MatchString(tc) {
			t.Errorf("pattern should match %q", tc)
		}
	}
}

func TestWriteBlockedSSE(t *testing.T) {
	g := &NenyaGateway{
		logger: setupLogger(false),
	}

	w := httptest.NewRecorder()
	g.writeBlockedSSE(w)

	body := w.Body.String()
	if !strings.Contains(body, "data: ") {
		t.Errorf("expected SSE data line in output: %q", body)
	}
	if !strings.Contains(body, "[Response blocked by execution policy]") {
		t.Errorf("expected blocked message in output: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("expected SSE DONE sentinel in output: %q", body)
	}

	var chunk map[string]interface{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
			data := strings.TrimPrefix(line, "data: ")
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				t.Fatalf("failed to parse SSE JSON: %v", err)
			}
		}
	}
	if chunk == nil {
		t.Fatal("no SSE chunk found in output")
	}
	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		choice, ok := choices[0].(map[string]interface{})
		if !ok {
			t.Fatal("choice is not a map")
		}
		if choice["finish_reason"] != "stop" {
			t.Errorf("expected finish_reason=stop, got %v", choice["finish_reason"])
		}
	}
}

func TestValidatePatterns(t *testing.T) {
	logger := setupLogger(false)

	t.Run("all valid", func(t *testing.T) {
		err := validatePatterns("test", []string{`(?i)\bhello\b`, `\d+`}, logger)
		if err != nil {
			t.Errorf("expected no error, got %v", err)
		}
	})

	t.Run("invalid pattern", func(t *testing.T) {
		err := validatePatterns("test", []string{`[invalid`}, logger)
		if err == nil {
			t.Error("expected error for invalid pattern")
		}
		if !strings.Contains(err.Error(), "test[0]") {
			t.Errorf("error should reference pattern index, got: %v", err)
		}
	})

	t.Run("mixed valid and invalid", func(t *testing.T) {
		err := validatePatterns("test", []string{`\d+`, `(unclosed`, `(?i)ok`}, logger)
		if err == nil {
			t.Error("expected error for mixed patterns")
		}
		if !strings.Contains(err.Error(), "test[1]") {
			t.Errorf("error should reference index 1, got: %v", err)
		}
	})

	t.Run("empty patterns", func(t *testing.T) {
		err := validatePatterns("test", nil, logger)
		if err != nil {
			t.Errorf("expected no error for nil patterns, got %v", err)
		}
	})
}
