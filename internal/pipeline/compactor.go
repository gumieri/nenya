package pipeline

import (
	"strings"

	"nenya/internal/config"
)

func ApplyCompaction(messages []interface{}, cfg config.CompactionConfig) bool {
	if !cfg.Enabled {
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

	if cc.NormalizeLineEndings {
		result = NormalizeLineEndings(result)
	}

	if cc.TrimTrailingWhitespace {
		result = TrimTrailingWhitespace(result)
	}

	if cc.CollapseBlankLines {
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

	var lineBuf []rune
	for _, r := range s {
		if r == '\n' {
			lineBuf = trimTrailingRunes(lineBuf)
			for _, c := range lineBuf {
				b.WriteRune(c)
			}
			b.WriteRune('\n')
			lineBuf = lineBuf[:0]
			continue
		}
		lineBuf = append(lineBuf, r)
	}
	lineBuf = trimTrailingRunes(lineBuf)
	for _, c := range lineBuf {
		b.WriteRune(c)
	}
	return b.String()
}

func trimTrailingRunes(runes []rune) []rune {
	i := len(runes) - 1
	for i >= 0 && (runes[i] == ' ' || runes[i] == '\t') {
		i--
	}
	return runes[:i+1]
}

func CollapseBlankLines(s string) string {
	const maxBlankLines = 2

	var b strings.Builder
	b.Grow(len(s))

	blankCount := 0
	for _, r := range s {
		if r == '\n' {
			blankCount++
			if blankCount <= maxBlankLines+1 {
				b.WriteRune(r)
			}
			continue
		}
		blankCount = 0
		b.WriteRune(r)
	}

	return b.String()
}
