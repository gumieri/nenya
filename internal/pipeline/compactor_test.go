package pipeline

import (
	"testing"

	"nenya/internal/config"
)

func TestNormalizeLineEndings(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no change", "hello\nworld", "hello\nworld"},
		{"crlf to lf", "hello\r\nworld", "hello\nworld"},
		{"mixed", "a\r\nb\nc\r\n", "a\nb\nc\n"},
		{"empty", "", ""},
		{"no newlines", "hello", "hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeLineEndings(tt.in)
			if got != tt.want {
				t.Errorf("NormalizeLineEndings() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTrimTrailingWhitespace(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no trailing ws", "hello\nworld", "hello\nworld"},
		{"trailing spaces", "hello   \nworld", "hello\nworld"},
		{"trailing tabs", "hello\t\t\nworld", "hello\nworld"},
		{"mixed trailing", "hello \t \nworld", "hello\nworld"},
		{"empty", "", ""},
		{"only whitespace lines", "   \n\t\t\n", "\n\n"},
		{"preserve internal spaces", "hello world  \nkeep spaces", "hello world\nkeep spaces"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TrimTrailingWhitespace(tt.in)
			if got != tt.want {
				t.Errorf("TrimTrailingWhitespace() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCollapseBlankLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no blanks", "a\nb\nc", "a\nb\nc"},
		{"single blank preserved", "a\n\nb", "a\n\nb"},
		{"double blank preserved", "a\n\n\nb", "a\n\n\nb"},
		{"triple blank collapsed", "a\n\n\n\nb", "a\n\n\nb"},
		{"many collapsed", "a\n\n\n\n\n\n\n\nb", "a\n\n\nb"},
		{"blanks at start and end", "\n\n\na\n\n\n", "\n\n\na\n\n\n"},
		{"no newlines", "abc", "abc"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CollapseBlankLines(tt.in)
			if got != tt.want {
				t.Errorf("CollapseBlankLines() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCompactText(t *testing.T) {
	t.Run("full pipeline normalize trim collapse", func(t *testing.T) {
		cc := config.CompactionConfig{
			NormalizeLineEndings:   true,
			TrimTrailingWhitespace: true,
			CollapseBlankLines:     true,
		}
		in := "hello \r\n   \nworld   \n\n\n\n\nend  "
		want := "hello\n\nworld\n\n\nend"
		got := CompactText(in, cc)
		if got != want {
			t.Errorf("CompactText() = %q, want %q", got, want)
		}
	})

	t.Run("empty text", func(t *testing.T) {
		cc := config.CompactionConfig{
			NormalizeLineEndings:   true,
			TrimTrailingWhitespace: true,
			CollapseBlankLines:     true,
		}
		got := CompactText("", cc)
		if got != "" {
			t.Errorf("CompactText() = %q, want empty", got)
		}
	})
}

func TestApplyCompaction(t *testing.T) {
	cc := config.CompactionConfig{
		Enabled:                true,
		NormalizeLineEndings:   true,
		TrimTrailingWhitespace: true,
		CollapseBlankLines:     true,
	}

	t.Run("messages with string content", func(t *testing.T) {
		messages := []interface{}{
			map[string]interface{}{"role": "user", "content": "hello \r\n   \nworld   "},
			map[string]interface{}{"role": "assistant", "content": "ok  \n\n\n\n\nsure"},
		}
		if !ApplyCompaction(messages, cc) {
			t.Fatal("expected true")
		}
		want0 := "hello\n\nworld"
		if messages[0].(map[string]interface{})["content"] != want0 {
			t.Errorf("messages[0] = %q, want %q", messages[0].(map[string]interface{})["content"], want0)
		}
		want1 := "ok\n\n\nsure"
		if messages[1].(map[string]interface{})["content"] != want1 {
			t.Errorf("messages[1] = %q, want %q", messages[1].(map[string]interface{})["content"], want1)
		}
	})

	t.Run("messages with array content", func(t *testing.T) {
		messages := []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "hello  \r\n\n\n\nworld   "},
					map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "http://example.com/img.png"}},
				},
			},
		}
		if !ApplyCompaction(messages, cc) {
			t.Fatal("expected true")
		}
		parts := messages[0].(map[string]interface{})["content"].([]interface{})
		want := "hello\n\n\nworld"
		if parts[0].(map[string]interface{})["text"] != want {
			t.Errorf("parts[0].text = %q, want %q", parts[0].(map[string]interface{})["text"], want)
		}
	})

	t.Run("disabled returns false", func(t *testing.T) {
		disabled := config.CompactionConfig{Enabled: false}
		messages := []interface{}{
			map[string]interface{}{"content": "hello   \n\n\n\n\nworld"},
		}
		if ApplyCompaction(messages, disabled) {
			t.Fatal("expected false")
		}
	})

	t.Run("nil content skipped", func(t *testing.T) {
		messages := []interface{}{
			map[string]interface{}{"role": "user"},
		}
		if ApplyCompaction(messages, cc) {
			t.Fatal("expected false")
		}
	})

	t.Run("non-map entries skipped", func(t *testing.T) {
		messages := []interface{}{
			"not a map",
			42,
			map[string]interface{}{"content": "already clean"},
		}
		if ApplyCompaction(messages, cc) {
			t.Fatal("expected false")
		}
	})
}
