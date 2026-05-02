package pipeline

import (
	"encoding/json"
	"unicode/utf8"

	"nenya/config"

	"testing"
)

func FuzzCompactText(f *testing.F) {
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 100000 {
			return
		}
		result := CompactText(text, config.CompactionConfig{
			JSONMinify:             true,
			CollapseBlankLines:     true,
			TrimTrailingWhitespace: true,
			NormalizeLineEndings:   true,
			PruneStaleTools:        false,
			ToolProtectionWindow:   60,
			PruneThoughts:          false,
		})
		if len(result) > len(text)+100 {
			t.Errorf("result longer than input: input=%d, output=%d", len(text), len(result))
		}
	})
}

func FuzzNormalizeLineEndings(f *testing.F) {
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 10000 {
			return
		}
		result := NormalizeLineEndings(text)
		if len(result) > len(text)+100 {
			t.Errorf("result longer than input: input=%d, output=%d", len(text), len(result))
		}
	})
}

func FuzzTrimTrailingWhitespace(f *testing.F) {
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 10000 {
			return
		}
		result := TrimTrailingWhitespace(text)
		if len(result) > len(text)+1 {
			t.Errorf("result longer than input: input=%d, output=%d", len(text), len(result))
		}
	})
}

func FuzzCollapseBlankLines(f *testing.F) {
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 10000 {
			return
		}
		result := CollapseBlankLines(text)
		if len(result) > len(text) {
			t.Errorf("result longer than input: input=%d, output=%d", len(text), len(result))
		}
	})
}

func FuzzDetectCodeFences(f *testing.F) {
	f.Add("```go\nfmt.Println(\"hi\")\n```")
	f.Add("no fences here")
	f.Add("```python\ndef foo():\n    pass\n```")
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 10000 {
			return
		}
		spans := DetectCodeFences(text)
		for _, s := range spans {
			if s.Start > s.End {
				t.Errorf("invalid span: start=%d > end=%d", s.Start, s.End)
			}
			if s.Start < 0 {
				t.Errorf("negative start: %d", s.Start)
			}
			if s.End < 0 {
				t.Errorf("negative end: %d", s.End)
			}
		}
	})
}

func FuzzShannonEntropy(f *testing.F) {
	f.Fuzz(func(t *testing.T, token string) {
		if len(token) > 1000 || !utf8.ValidString(token) {
			return
		}
		result := ShannonEntropy(token)
		if result < 0 {
			t.Errorf("negative entropy: %f", result)
		}
	})
}

func FuzzRedactSecrets(f *testing.F) {
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 10000 {
			return
		}
		result := RedactSecrets(text, true, nil, "[REDACTED]")
		if result != text {
			t.Errorf("unexpected change with no patterns: input=%d, output=%d", len(text), len(result))
		}
	})
}

func FuzzExtractContentText(f *testing.F) {
	f.Add([]byte(`{"role":"user","content":"hello"}`))
	f.Add([]byte(`{"role":"assistant","content":[{"type":"text","text":"hi"}]}`))
	f.Fuzz(func(t *testing.T, jsonBytes []byte) {
		var msg map[string]interface{}
		if err := json.Unmarshal(jsonBytes, &msg); err != nil {
			return
		}
		result := ExtractContentText(msg)
		if len(result) > 100000 {
			t.Errorf("unexpectedly long content: %d bytes", len(result))
		}
	})
}

func FuzzSerializeMessages(f *testing.F) {
	f.Add([]byte(`[{"role":"user","content":"hello"}]`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, jsonBytes []byte) {
		var messages []interface{}
		if err := json.Unmarshal(jsonBytes, &messages); err != nil {
			return
		}
		result := SerializeMessages(messages)
		if len(result) > 10*1024*1024 {
			t.Errorf("result unreasonably large: %d bytes", len(result))
		}
	})
}
