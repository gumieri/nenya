package pipeline

import (
	"strings"
	"testing"
)

func TestDetectCodeFences(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		want   int
		lang   string
	}{
		{"no fences", "hello world", 0, ""},
		{"single fence", "before\n```go\ncode\n```\nafter", 1, "go"},
		{"no language", "before\n```\ncode\n```\nafter", 1, ""},
		{"two fences", "```python\na\n```\nb\n```js\nc\n```", 2, ""},
		{"unmatched open", "```go\nno close", 0, ""},
		{"unmatched close", "no open\n```\n", 0, ""},
		{"four backticks", "````yaml\na\n````\n", 1, "yaml"},
		{"empty fence", "```\n```\n", 1, ""},
		{"fence in middle of line ignored", "hello ``` not a fence", 0, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectCodeFences(tt.text)
			if len(got) != tt.want {
				t.Errorf("got %d spans, want %d", len(got), tt.want)
				return
			}
			if tt.want > 0 && tt.lang != "" {
				if got[0].Language != tt.lang {
					t.Errorf("language = %q, want %q", got[0].Language, tt.lang)
				}
			}
		})
	}
}

func TestDetectCodeFences_SpanBoundaries(t *testing.T) {
	text := "before\n```go\nfunc main() {}\n```\nafter"
	spans := DetectCodeFences(text)
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	span := spans[0]
	if span.Language != "go" {
		t.Errorf("language = %q, want %q", span.Language, "go")
	}
	if text[span.Start:span.Start+6] != "```go\n" {
		t.Errorf("span start mismatch: %q", text[span.Start:span.Start+6])
	}
	if !strings.HasSuffix(text[span.Start:span.End], "```") {
		t.Errorf("span end mismatch: %q", text[span.Start:span.End])
	}
	inner := text[span.Start:span.End]
	if !contains(inner, "func main() {}") {
		t.Errorf("span does not contain code body: %q", inner)
	}
}

func TestHasCodeFences(t *testing.T) {
	if HasCodeFences("no code here") {
		t.Error("expected false")
	}
	if !HasCodeFences("```go\ncode\n```") {
		t.Error("expected true")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
