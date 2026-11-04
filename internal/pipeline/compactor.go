package pipeline

import (
	"strings"

	"github.com/nenya/config"
)

func ApplyCompaction(messages []interface{}, cfg config.CompactionConfig) bool {
	if cfg.Enabled == nil || !*cfg.Enabled {
		return false
	}

	mutated := false

	for _, msgRaw := range messages {
		msgNode, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}

		content := msgNode["content"]
		if content == nil {
			continue
		}

		switch c := content.(type) {
		case string:
			if compacted := CompactText(c, cfg); compacted != c {
				msgNode["content"] = compacted
				mutated = true
			}
		case []interface{}:
			for _, partRaw := range c {
				part, ok := partRaw.(map[string]interface{})
				if !ok {
					continue
				}
				text, ok := part["text"].(string)
				if !ok || text == "" {
					continue
				}
				if compacted := CompactText(text, cfg); compacted != text {
					part["text"] = compacted
					mutated = true
				}
			}
		}
	}

	return mutated
}

func CompactText(text string, cc config.CompactionConfig) string {
	result := text

	if cc.NormalizeLineEndings != nil && *cc.NormalizeLineEndings {
		result = NormalizeLineEndings(result)
	}

	if cc.TrimTrailingWhitespace != nil && *cc.TrimTrailingWhitespace {
		result = TrimTrailingWhitespace(result)
	}

	if cc.CollapseBlankLines != nil && *cc.CollapseBlankLines {
		result = CollapseBlankLines(result)
	}

	return result
}

func NormalizeLineEndings(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

func TrimTrailingWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	// Byte-oriented: '\n' cannot appear inside a multi-byte UTF-8 sequence,
	// so byte iteration is equivalent to rune iteration for valid text while
	// preserving invalid bytes exactly (range-over-string would re-encode
	// each invalid byte as U+FFFD, GROWING the output — caught by
	// FuzzTrimTrailingWhitespace).
	var lineBuf []byte
	flush := func() {
		lineBuf = trimTrailingBytes(lineBuf)
		b.Write(lineBuf)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' {
			flush()
			b.WriteByte('\n')
			lineBuf = lineBuf[:0]
			continue
		}
		lineBuf = append(lineBuf, c)
	}
	flush()
	return b.String()
}

func trimTrailingBytes(b []byte) []byte {
	i := len(b) - 1
	for i >= 0 && (b[i] == ' ' || b[i] == '\t') {
		i--
	}
	return b[:i+1]
}

func CollapseBlankLines(s string) string {
	const maxBlankLines = 2

	var b strings.Builder
	b.Grow(len(s))

	// Byte-oriented (see TrimTrailingWhitespace): preserves invalid UTF-8
	// bytes exactly instead of re-encoding them as U+FFFD.
	blankCount := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' {
			blankCount++
			if blankCount <= maxBlankLines+1 {
				b.WriteByte(c)
			}
			continue
		}
		blankCount = 0
		b.WriteByte(c)
	}

	return b.String()
}
