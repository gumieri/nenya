package stream

import (
	"regexp"
	"strings"
	"testing"
)

func TestFilterContentPass(t *testing.T) {
	f := NewStreamFilter(
		[]*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
		[]*regexp.Regexp{regexp.MustCompile(`(?i)rm -rf /`)},
		"[REDACTED]",
		0,
	)
	out, action, reason := f.FilterContent("hello world")
	if out != "hello world" || action != ActionPass || reason != "" {
		t.Fatalf("pass: got (%q, %d, %q)", out, action, reason)
	}
}

func TestFilterContentRedact(t *testing.T) {
	f := NewStreamFilter(
		[]*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
		nil,
		"[REDACTED]",
		0,
	)
	out, action, _ := f.FilterContent("key is AKIAIOSFODNN7EXAMPLE here")
	if action != ActionRedact {
		t.Fatalf("expected ActionRedact, got %d", action)
	}
	if strings.Contains(out, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("secret not redacted: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("redact label missing: %s", out)
	}
}

func TestFilterContentBlock(t *testing.T) {
	f := NewStreamFilter(
		nil,
		[]*regexp.Regexp{regexp.MustCompile(`(?i)rm -rf /`)},
		"[REDACTED]",
		0,
	)
	out, action, reason := f.FilterContent("run rm -rf / now")
	if action != ActionBlock {
		t.Fatalf("expected ActionBlock, got %d", action)
	}
	if reason == "" {
		t.Fatal("expected block reason")
	}
	if out != "run rm -rf / now" {
		t.Fatalf("blocked content should pass through unchanged: %s", out)
	}
}

func TestFilterContentBlockPrecedenceOverRedact(t *testing.T) {
	f := NewStreamFilter(
		[]*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
		[]*regexp.Regexp{regexp.MustCompile(`(?i)rm -rf /`)},
		"[REDACTED]",
		0,
	)
	out, action, _ := f.FilterContent("AKIAIOSFODNN7EXAMPLE and rm -rf /")
	if action != ActionBlock {
		t.Fatalf("expected ActionBlock, got %d", action)
	}
	if !strings.Contains(out, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatal("blocked content should not be redacted")
	}
}

func TestFilterContentOnceBlockedStaysBlocked(t *testing.T) {
	f := NewStreamFilter(
		nil,
		[]*regexp.Regexp{regexp.MustCompile(`(?i)rm -rf /`)},
		"[REDACTED]",
		0,
	)
	f.FilterContent("rm -rf /")
	if !f.IsBlocked() {
		t.Fatal("should be blocked after first block")
	}
	out, action, reason := f.FilterContent("innocent text")
	if action != ActionBlock {
		t.Fatalf("expected ActionBlock on subsequent call, got %d", action)
	}
	if reason == "" {
		t.Fatal("expected block reason on subsequent call")
	}
	if out != "innocent text" {
		t.Fatalf("subsequent blocked content should pass through: %s", out)
	}
}

func TestFilterContentEmpty(t *testing.T) {
	f := NewStreamFilter(
		[]*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
		[]*regexp.Regexp{regexp.MustCompile(`(?i)rm -rf /`)},
		"[REDACTED]",
		0,
	)
	out, action, reason := f.FilterContent("")
	if out != "" || action != ActionPass || reason != "" {
		t.Fatalf("empty: got (%q, %d, %q)", out, action, reason)
	}
}

func TestFilterContentNilPatterns(t *testing.T) {
	f := NewStreamFilter(nil, nil, "[REDACTED]", 0)
	out, action, reason := f.FilterContent("anything goes")
	if out != "anything goes" || action != ActionPass || reason != "" {
		t.Fatalf("nil patterns: got (%q, %d, %q)", out, action, reason)
	}
}

func TestIsBlocked(t *testing.T) {
	f := NewStreamFilter(nil, []*regexp.Regexp{regexp.MustCompile(`block`)}, "", 0)
	if f.IsBlocked() {
		t.Fatal("should not be blocked initially")
	}
	f.FilterContent("block")
	if !f.IsBlocked() {
		t.Fatal("should be blocked after block")
	}
}

func TestWindowLen(t *testing.T) {
	f := NewStreamFilter(nil, nil, "", 100)
	if f.WindowLen() != 0 {
		t.Fatalf("initial window len: got %d", f.WindowLen())
	}
	f.FilterContent("abc")
	if f.WindowLen() != 3 {
		t.Fatalf("after 'abc': got %d", f.WindowLen())
	}
	f.FilterContent("de")
	if f.WindowLen() != 5 {
		t.Fatalf("after 'de': got %d", f.WindowLen())
	}
}

func TestWindowContent(t *testing.T) {
	f := NewStreamFilter(nil, nil, "", 100)
	f.FilterContent("hello")
	if f.WindowContent() != "hello" {
		t.Fatalf("got %q", f.WindowContent())
	}
	f.FilterContent(" world")
	if f.WindowContent() != "hello world" {
		t.Fatalf("got %q", f.WindowContent())
	}
}

func TestDefaultWindowSize(t *testing.T) {
	f := NewStreamFilter(nil, nil, "", 0)
	if f.WindowLen() != 0 {
		t.Fatalf("initial: got %d", f.WindowLen())
	}
	long := strings.Repeat("x", 5000)
	f.FilterContent(long)
	if f.WindowLen() != 4096 {
		t.Fatalf("expected 4096, got %d", f.WindowLen())
	}
}

func TestDefaultWindowSizeNegative(t *testing.T) {
	f := NewStreamFilter(nil, nil, "", -10)
	long := strings.Repeat("x", 5000)
	f.FilterContent(long)
	if f.WindowLen() != 4096 {
		t.Fatalf("expected 4096 for negative input, got %d", f.WindowLen())
	}
}

func TestSlidingWindowCrossChunkSecret(t *testing.T) {
	re := regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	f := NewStreamFilter([]*regexp.Regexp{re}, nil, "[REDACTED]", 30)

	f.FilterContent("key is AKIA")
	out, action, _ := f.FilterContent("IOSFODNN7EXAMPLE here")

	if action != ActionRedact {
		t.Fatalf("expected ActionRedact, got %d", action)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("cross-chunk secret not redacted: %s", out)
	}
}

func TestSlidingWindowCrossChunkBlock(t *testing.T) {
	re := regexp.MustCompile(`(?i)rm -rf /`)
	f := NewStreamFilter(nil, []*regexp.Regexp{re}, "", 20)

	f.FilterContent("please rm -rf")
	_, action, reason := f.FilterContent(" /")

	if action != ActionBlock {
		t.Fatalf("expected ActionBlock, got %d", action)
	}
	if reason == "" {
		t.Fatal("expected block reason")
	}
}

func TestSlidingWindowEviction(t *testing.T) {
	f := NewStreamFilter(nil, nil, "", 10)
	f.FilterContent("0123456789")
	f.FilterContent("ABCDE")
	if f.WindowContent() != "56789ABCDE" {
		t.Fatalf("expected '56789ABCDE', got %q", f.WindowContent())
	}
	if f.WindowLen() != 10 {
		t.Fatalf("expected 10, got %d", f.WindowLen())
	}
}

func TestSlidingWindowEvictionLargeChunk(t *testing.T) {
	f := NewStreamFilter(nil, nil, "", 5)
	f.FilterContent("0123456789ABCDEF")
	if f.WindowLen() != 5 {
		t.Fatalf("expected windowLen=5, got %d", f.WindowLen())
	}
}

func TestWindowTooSmallForCrossChunk(t *testing.T) {
	re := regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	f := NewStreamFilter([]*regexp.Regexp{re}, nil, "[REDACTED]", 5)

	f.FilterContent("AKIA")
	_, action, _ := f.FilterContent("1234567890ABCDEF")

	if action == ActionRedact {
		t.Fatal("window too small, should not detect cross-chunk secret")
	}
}

func TestExtractDeltaContent(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "valid delta",
			data: []byte(`{"choices":[{"delta":{"content":"hello"}}]}`),
			want: "hello",
		},
		{
			name: "empty choices",
			data: []byte(`{"choices":[]}`),
			want: "",
		},
		{
			name: "no choices key",
			data: []byte(`{"id":"1"}`),
			want: "",
		},
		{
			name: "no delta key",
			data: []byte(`{"choices":[{"finish_reason":"stop"}]}`),
			want: "",
		},
		{
			name: "delta has no content",
			data: []byte(`{"choices":[{"delta":{"role":"assistant"}}]}`),
			want: "",
		},
		{
			name: "invalid json",
			data: []byte(`not json`),
			want: "",
		},
		{
			name: "empty data",
			data: []byte{},
			want: "",
		},
		{
			name: "null data",
			data: nil,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractDeltaContent(tt.data)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReplaceDeltaContent(t *testing.T) {
	tests := []struct {
		name       string
		data       []byte
		newContent string
		wantSubstr string
	}{
		{
			name:       "replace content",
			data:       []byte(`{"choices":[{"delta":{"content":"secret"}}]}`),
			newContent: "[REDACTED]",
			wantSubstr: `[REDACTED]`,
		},
		{
			name:       "replace with empty",
			data:       []byte(`{"choices":[{"delta":{"content":"hello"}}]}`),
			newContent: "",
			wantSubstr: `"content":""`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReplaceDeltaContent(tt.data, tt.newContent)
			if !strings.Contains(string(got), tt.wantSubstr) {
				t.Fatalf("got %s, want substr %q", string(got), tt.wantSubstr)
			}
		})
	}
}

func TestReplaceDeltaContentInvalidJSON(t *testing.T) {
	data := []byte(`not json`)
	got := ReplaceDeltaContent(data, "x")
	if string(got) != "not json" {
		t.Fatalf("invalid json should return original: %s", string(got))
	}
}

func TestReplaceDeltaContentNoChoices(t *testing.T) {
	data := []byte(`{"id":"1"}`)
	got := ReplaceDeltaContent(data, "x")
	if !strings.Contains(string(got), `"id":"1"`) {
		t.Fatalf("expected id preserved, got %s", string(got))
	}
}

func TestParseSSEChunk(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		wantNil bool
	}{
		{
			name:    "valid json",
			data:    []byte(`{"id":"1","choices":[]}`),
			wantNil: false,
		},
		{
			name:    "invalid json",
			data:    []byte(`{broken`),
			wantNil: true,
		},
		{
			name:    "empty",
			data:    []byte{},
			wantNil: true,
		},
		{
			name:    "null",
			data:    nil,
			wantNil: true,
		},
		{
			name:    "json null",
			data:    []byte(`null`),
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseSSEChunk(tt.data)
			if tt.wantNil && got != nil {
				t.Fatal("expected nil")
			}
			if !tt.wantNil && got == nil {
				t.Fatal("expected non-nil")
			}
		})
	}
}
