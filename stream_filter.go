package main

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

var ErrStreamBlocked = errors.New("stream blocked by execution policy")

type FilterAction int

const (
	ActionPass FilterAction = iota
	ActionRedact
	ActionBlock
)

type StreamFilter struct {
	secretPatterns []*regexp.Regexp
	blockPatterns  []*regexp.Regexp
	redactLabel    string
	window         []rune
	windowSize     int
	windowLen      int
	blocked        bool
	blockReason    string
}

func NewStreamFilter(secretPatterns, blockPatterns []*regexp.Regexp, redactLabel string, windowSize int) *StreamFilter {
	if windowSize <= 0 {
		windowSize = 4096
	}
	return &StreamFilter{
		secretPatterns: secretPatterns,
		blockPatterns:  blockPatterns,
		redactLabel:    redactLabel,
		window:         make([]rune, 0, windowSize),
		windowSize:     windowSize,
	}
}

func (f *StreamFilter) FilterContent(content string) (string, FilterAction, string) {
	if f.blocked {
		return content, ActionBlock, f.blockReason
	}

	if len(content) == 0 {
		return content, ActionPass, ""
	}

	if len(f.blockPatterns) > 0 {
		for _, re := range f.blockPatterns {
			if re.MatchString(content) {
				f.blocked = true
				f.blockReason = re.String()
				return content, ActionBlock, f.blockReason
			}
		}
	}

	prevWindowLen := f.windowLen

	f.appendToWindow(content)

	if f.checkWindowBlock() {
		return content, ActionBlock, f.blockReason
	}

	var redacted string
	var wasRedacted bool

	if len(f.secretPatterns) > 0 {
		redacted = content
		for _, re := range f.secretPatterns {
			if re.MatchString(redacted) {
				redacted = re.ReplaceAllString(redacted, f.redactLabel)
				wasRedacted = true
			}
		}
	}

	if wasRedacted {
		return redacted, ActionRedact, ""
	}

	if f.checkWindowRedact() {
		redacted = content
		windowStr := string(f.window)
		for _, re := range f.secretPatterns {
			locs := re.FindAllStringIndex(windowStr, -1)
			for _, loc := range locs {
				chunkStart := loc[0] - prevWindowLen
				if chunkStart < 0 {
					chunkStart = 0
				}
				chunkEnd := loc[1] - prevWindowLen
				if chunkEnd > len(content) {
					chunkEnd = len(content)
				}
				if chunkStart < len(content) && chunkEnd > chunkStart {
					prefix := content[:chunkStart]
					suffix := content[chunkEnd:]
					redacted = prefix + f.redactLabel + suffix
					content = redacted
				}
			}
		}
		if strings.Contains(redacted, f.redactLabel) {
			return redacted, ActionRedact, ""
		}
		return content, ActionPass, ""
	}

	return content, ActionPass, ""
}

func (f *StreamFilter) appendToWindow(text string) {
	runes := []rune(text)
	total := f.windowLen + len(runes)
	if total <= f.windowSize {
		f.window = append(f.window, runes...)
		f.windowLen = total
		return
	}
	if total > f.windowSize {
		drop := total - f.windowSize
		if drop >= f.windowLen {
			f.window = f.window[:0]
		} else {
			f.window = f.window[drop:]
		}
		f.window = append(f.window, runes...)
		f.windowLen = f.windowSize
	}
}

func (f *StreamFilter) checkWindowBlock() bool {
	if len(f.blockPatterns) == 0 || f.windowLen == 0 {
		return false
	}
	windowStr := string(f.window)
	for _, re := range f.blockPatterns {
		if re.MatchString(windowStr) {
			f.blocked = true
			f.blockReason = re.String()
			return true
		}
	}
	return false
}

func (f *StreamFilter) checkWindowRedact() bool {
	if len(f.secretPatterns) == 0 || f.windowLen == 0 {
		return false
	}
	windowStr := string(f.window)
	for _, re := range f.secretPatterns {
		matched := re.MatchString(windowStr)
		if matched {
			return true
		}
	}
	return false
}

func (f *StreamFilter) IsBlocked() bool {
	return f.blocked
}

func (f *StreamFilter) WindowContent() string {
	return string(f.window)
}

func (f *StreamFilter) WindowLen() int {
	return f.windowLen
}

func extractDeltaContent(data []byte) string {
	if len(data) == 0 {
		return ""
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return ""
	}

	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return ""
	}

	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return ""
	}

	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return ""
	}

	content, _ := delta["content"].(string)
	return content
}

func replaceDeltaContent(data []byte, newContent string) []byte {
	var chunk map[string]interface{}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return data
	}

	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return data
	}

	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return data
	}

	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return data
	}

	delta["content"] = newContent

	result, err := json.Marshal(chunk)
	if err != nil {
		return data
	}
	return result
}
