package stream

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

	if action, reason := f.checkBlockPatterns(content); action != ActionPass {
		return content, action, reason
	}

	prevWindowBytes := len(string(f.window))
	f.appendToWindow(content)

	if f.checkWindowBlock() {
		return content, ActionBlock, f.blockReason
	}

	if redacted, action := f.checkSecretPatterns(content); action != ActionPass {
		return redacted, action, ""
	}

	if f.checkWindowRedact() {
		return f.redactFromWindow(content, prevWindowBytes)
	}

	return content, ActionPass, ""
}

func (f *StreamFilter) checkBlockPatterns(content string) (FilterAction, string) {
	if len(f.blockPatterns) == 0 {
		return ActionPass, ""
	}
	for _, re := range f.blockPatterns {
		if re.MatchString(content) {
			f.blocked = true
			f.blockReason = re.String()
			return ActionBlock, f.blockReason
		}
	}
	return ActionPass, ""
}

func (f *StreamFilter) checkSecretPatterns(content string) (string, FilterAction) {
	if len(f.secretPatterns) == 0 {
		return content, ActionPass
	}

	redacted := content
	wasRedacted := false
	for _, re := range f.secretPatterns {
		if re.MatchString(redacted) {
			redacted = re.ReplaceAllString(redacted, f.redactLabel)
			wasRedacted = true
		}
	}

	if wasRedacted {
		return redacted, ActionRedact
	}
	return content, ActionPass
}

func (f *StreamFilter) redactFromWindow(content string, prevWindowBytes int) (string, FilterAction, string) {
	redacted := content
	windowStr := string(f.window)
	for _, re := range f.secretPatterns {
		locs := re.FindAllStringIndex(windowStr, -1)
		for _, loc := range locs {
			chunkStart := loc[0] - prevWindowBytes
			if chunkStart < 0 {
				chunkStart = 0
			}
			chunkEnd := loc[1] - prevWindowBytes
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

func (f *StreamFilter) appendToWindow(text string) {
	f.windowLen = AppendRuneWindow(&f.window, &f.windowLen, f.windowSize, text)
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
		if re.MatchString(windowStr) {
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

// ExtractDeltaContent extracts the delta content from SSE chunk data by parsing
// the JSON and navigating the OpenAI-style structure: choices[0].delta.content.
// Returns an empty string if the structure is invalid or the data is not valid JSON.
func ExtractDeltaContent(data []byte) string {
	return ExtractDeltaContentFromMap(ParseSSEChunk(data))
}

// ExtractDeltaContentFromMap extracts the delta content field from a parsed SSE chunk.
// It navigates the OpenAI-style structure: choices[0].delta.content and returns the
// content string, or an empty string if the structure is invalid.
func ExtractDeltaContentFromMap(chunk map[string]interface{}) string {
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

func ReplaceDeltaContent(data []byte, newContent string) []byte {
	chunk := ParseSSEChunk(data)
	if chunk == nil {
		return data
	}
	return ReplaceDeltaContentMap(chunk, newContent)
}

// ReplaceDeltaContentMap replaces the delta content field in a parsed SSE chunk with
// newContent and returns the modified chunk as JSON bytes. If the chunk structure is
// invalid, it returns the original chunk marshaled.
func ReplaceDeltaContentMap(chunk map[string]interface{}, newContent string) []byte {
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		result, _ := json.Marshal(chunk)
		return result
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		result, _ := json.Marshal(chunk)
		return result
	}
	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		result, _ := json.Marshal(chunk)
		return result
	}
	original := delta["content"]
	delta["content"] = newContent
	result, err := json.Marshal(chunk)
	if err != nil {
		delta["content"] = original
		result, _ = json.Marshal(chunk)
	}
	return result
}

// ParseSSEChunk parses SSE chunk JSON data into a map. Returns nil if the data
// is not valid JSON.
func ParseSSEChunk(data []byte) map[string]interface{} {
	var chunk map[string]interface{}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil
	}
	return chunk
}
