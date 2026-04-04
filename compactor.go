package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

func (g *NenyaGateway) applyCompaction(messages []interface{}) bool {
	if !g.config.Compaction.Enabled {
		return false
	}

	cc := &g.config.Compaction
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
			if compacted := g.compactText(c, cc); compacted != c {
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
				if compacted := g.compactText(text, cc); compacted != text {
					part["text"] = compacted
					mutated = true
				}
			}
		}
	}

	return mutated
}

func (g *NenyaGateway) compactText(text string, cc *CompactionConfig) string {
	result := text

	if cc.NormalizeLineEndings {
		result = normalizeLineEndings(result)
	}

	if cc.TrimTrailingWhitespace {
		result = trimTrailingWhitespace(result)
	}

	if cc.CollapseBlankLines {
		result = collapseBlankLines(result)
	}

	return result
}

func (g *NenyaGateway) minifyJSON(body []byte) ([]byte, error) {
	if !g.config.Compaction.Enabled || !g.config.Compaction.JSONMinify {
		return body, nil
	}

	var buf bytes.Buffer
	if err := json.Compact(&buf, body); err != nil {
		return body, err
	}
	return buf.Bytes(), nil
}

func normalizeLineEndings(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

func trimTrailingWhitespace(s string) string {
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

func collapseBlankLines(s string) string {
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
