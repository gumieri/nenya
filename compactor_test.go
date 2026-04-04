package main

import (
	"log/slog"
	"testing"
)

func TestNormalizeLineEndings(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no change", "hello\nworld", "hello\nworld"},
		{"crlf to lf", "hello\r\nworld", "hello\nworld"},
		{"mixed", "a\r\nb\nc\r\nd", "a\nb\nc\nd"},
		{"empty", "", ""},
		{"no newlines", "hello", "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeLineEndings(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTrimTrailingWhitespace(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no trailing ws", "hello\nworld", "hello\nworld"},
		{"trailing spaces", "hello   \nworld   ", "hello\nworld"},
		{"trailing tabs", "hello\t\t\nworld\t", "hello\nworld"},
		{"mixed", "line one   \nline two\t\t\nline three", "line one\nline two\nline three"},
		{"empty", "", ""},
		{"only whitespace lines", "   \n\t\t\n", "\n\n"},
		{"preserve internal spaces", "hello world   \nfoo bar   ", "hello world\nfoo bar"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := trimTrailingWhitespace(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCollapseBlankLines(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no blanks", "hello\nworld", "hello\nworld"},
		{"single blank preserved", "hello\n\nworld", "hello\n\nworld"},
		{"double blank preserved", "hello\n\n\nworld", "hello\n\n\nworld"},
		{"triple collapsed", "hello\n\n\n\nworld", "hello\n\n\nworld"},
		{"many collapsed", "a\n\n\n\n\n\n\nb", "a\n\n\nb"},
		{"blanks at start", "\n\n\n\nhello", "\n\n\nhello"},
		{"blanks at end", "hello\n\n\n\n", "hello\n\n\n"},
		{"no newlines", "hello", "hello"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collapseBlankLines(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCompactText(t *testing.T) {
	cfg := Config{
		Compaction: CompactionConfig{
			Enabled:                true,
			NormalizeLineEndings:   true,
			TrimTrailingWhitespace: true,
			CollapseBlankLines:     true,
		},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	input := "line one   \r\nline two   \n\n\n\nline three   \n"
	want := "line one\nline two\n\n\nline three\n"

	got := g.compactText(input, &g.config.Compaction)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCompactTextEmpty(t *testing.T) {
	cfg := Config{
		Compaction: CompactionConfig{
			Enabled:                true,
			NormalizeLineEndings:   true,
			TrimTrailingWhitespace: true,
			CollapseBlankLines:     true,
		},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	if got := g.compactText("", &g.config.Compaction); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestMinifyJSON(t *testing.T) {
	cfg := Config{
		Compaction: CompactionConfig{
			Enabled:    true,
			JSONMinify: true,
		},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	input := []byte(`{  "hello"  :  "world"  }`)
	want := []byte(`{"hello":"world"}`)

	got, err := g.minifyJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMinifyJSONDisabled(t *testing.T) {
	cfg := Config{
		Compaction: CompactionConfig{
			Enabled:    false,
			JSONMinify: true,
		},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	input := []byte(`{  "hello"  :  "world"  }`)
	got, err := g.minifyJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(input) {
		t.Errorf("expected passthrough when disabled, got %q", got)
	}
}

func TestApplyCompactionOnMessages(t *testing.T) {
	cfg := Config{
		Compaction: CompactionConfig{
			Enabled:                true,
			NormalizeLineEndings:   true,
			TrimTrailingWhitespace: true,
			CollapseBlankLines:     true,
		},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	messages := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": "hello   \r\n\r\n\r\n\r\nworld   ",
		},
		map[string]interface{}{
			"role": "assistant",
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "response   \n"},
			},
		},
	}

	mutated := g.applyCompaction(messages)
	if !mutated {
		t.Error("expected mutation")
	}

	textContent := messages[0].(map[string]interface{})["content"].(string)
	if textContent != "hello\n\n\nworld" {
		t.Errorf("got %q", textContent)
	}

	parts := messages[1].(map[string]interface{})["content"].([]interface{})
	partText := parts[0].(map[string]interface{})["text"].(string)
	if partText != "response\n" {
		t.Errorf("got %q", partText)
	}
}

func TestApplyCompactionDisabled(t *testing.T) {
	cfg := Config{
		Compaction: CompactionConfig{
			Enabled: false,
		},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	messages := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": "hello   \r\nworld   ",
		},
	}

	if g.applyCompaction(messages) {
		t.Error("expected no mutation when disabled")
	}
}

func TestApplyCompactionNilContent(t *testing.T) {
	cfg := Config{
		Compaction: CompactionConfig{
			Enabled:                true,
			NormalizeLineEndings:   true,
			TrimTrailingWhitespace: true,
			CollapseBlankLines:     true,
		},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	messages := []interface{}{
		map[string]interface{}{"role": "user", "content": nil},
		map[string]interface{}{"role": "assistant"},
		map[string]interface{}{"role": "user", "content": "hello   \r\nworld   "},
	}

	mutated := g.applyCompaction(messages)
	if !mutated {
		t.Error("expected mutation")
	}

	textContent := messages[2].(map[string]interface{})["content"].(string)
	if textContent != "hello\nworld" {
		t.Errorf("got %q", textContent)
	}
}

func TestApplyCompactionMultiPartNonTextOnly(t *testing.T) {
	cfg := Config{
		Compaction: CompactionConfig{
			Enabled:                true,
			NormalizeLineEndings:   true,
			TrimTrailingWhitespace: true,
			CollapseBlankLines:     true,
		},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	messages := []interface{}{
		map[string]interface{}{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "image_url", "url": "http://example.com/img.png"},
			},
		},
	}

	mutated := g.applyCompaction(messages)
	if mutated {
		t.Error("expected no mutation when only non-text parts")
	}
}

func TestMinifyJSONInvalidInput(t *testing.T) {
	cfg := Config{
		Compaction: CompactionConfig{
			Enabled:    true,
			JSONMinify: true,
		},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	input := []byte(`{invalid json}`)
	got, err := g.minifyJSON(input)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if string(got) != string(input) {
		t.Errorf("expected original returned on invalid JSON, got %q", got)
	}
}
