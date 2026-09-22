package stream

import (
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/nenya/internal/pipeline"
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

	f.appendToWindow(content)
	// Capture the window AFTER appending so straddling matches span the
	// prior window plus this chunk; the chunk's byte offset inside the
	// window is the difference (clamped: a chunk larger than the window
	// evicted everything, so the chunk starts at 0).
	windowStr := string(f.window)
	prevWindowBytes := len(windowStr) - len(content)
	if prevWindowBytes < 0 {
		prevWindowBytes = 0
	}

	if f.checkWindowBlock(windowStr) {
		return content, ActionBlock, f.blockReason
	}

	return f.processSecrets(content, windowStr, prevWindowBytes)
}

// processSecrets applies boundary-straddling (window) redaction first, on
// the pristine chunk, then fully in-chunk pattern redaction. Running the
// window pass first prevents an in-chunk replacement from consuming a
// straddling match's in-chunk fragment — which would leak the secret once
// the client concatenates chunks.
func (f *StreamFilter) processSecrets(content, windowStr string, prevWindowBytes int) (string, FilterAction, string) {
	action := ActionPass
	var reason string
	if f.checkWindowRedact(windowStr) {
		var a FilterAction
		content, a, reason = f.redactFromWindow(content, windowStr, prevWindowBytes)
		if a == ActionRedact {
			action = a
		}
	}
	redacted, a := f.checkSecretPatterns(content)
	if a != ActionPass {
		content = redacted
		action = a
	}
	return content, action, reason
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
		next := pipeline.ReplaceValidated(re, redacted, f.redactLabel)
		if next != redacted {
			wasRedacted = true
			redacted = next
		}
	}

	if wasRedacted {
		return redacted, ActionRedact
	}
	return content, ActionPass
}

// redactFromWindow redacts matches that straddle the window boundary. All
// match ranges are computed against the pristine chunk and spliced in a
// single left-to-right pass so progressive mutation cannot corrupt later
// offsets. A checksum-gated match fully inside the fresh chunk is skipped
// when validation fails; a straddling match fuses prior-window text with
// this chunk's text, so validation there is unreliable and the match is
// redacted ungated — over-redaction is fail-safe, a leaked fragment is not.
// windowSpan is a redaction range in content coordinates for a secret
// match straddling the stream window boundary.
type windowSpan struct{ start, end int }

// collectWindowSpans finds straddling secret matches in content
// coordinates. Checksum-gated matches fully inside the fresh chunk are
// skipped when validation fails; straddling matches fall back to ungated
// replacement. Returns the spans and the first matching pattern source.
func (f *StreamFilter) collectWindowSpans(windowStr string, prevWindowBytes, contentLen int) (spans []windowSpan, reason string) {
	for _, re := range f.secretPatterns {
		validate, hasValidator := pipeline.MatchValidator(re.String())
		for _, loc := range re.FindAllStringIndex(windowStr, -1) {
			if hasValidator && !validate(windowStr[loc[0]:loc[1]]) && loc[0] >= prevWindowBytes {
				continue
			}
			start := loc[0] - prevWindowBytes
			if start < 0 {
				start = 0
			}
			end := loc[1] - prevWindowBytes
			if end > contentLen {
				end = contentLen
			}
			if start < end {
				spans = append(spans, windowSpan{start, end})
				if reason == "" {
					// Pattern source, never the matched text — reason
					// values surface in logs and metrics.
					reason = re.String()
				}
			}
		}
	}
	return spans, reason
}

func (f *StreamFilter) redactFromWindow(content, windowStr string, prevWindowBytes int) (string, FilterAction, string) {
	spans, reason := f.collectWindowSpans(windowStr, prevWindowBytes, len(content))
	if len(spans) == 0 {
		return content, ActionPass, ""
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var b strings.Builder
	last := 0
	for _, s := range spans {
		if s.start < last {
			// Overlapping match: extend the redacted region so a longer
			// overlapping span cannot leak its tail.
			if s.end > last {
				last = s.end
			}
			continue
		}
		if s.start > last {
			b.WriteString(content[last:s.start])
		}
		b.WriteString(f.redactLabel)
		last = s.end
	}
	if last < len(content) {
		b.WriteString(content[last:])
	}
	return b.String(), ActionRedact, reason
}

func (f *StreamFilter) appendToWindow(text string) {
	f.windowLen = AppendRuneWindow(&f.window, &f.windowLen, f.windowSize, text)
}

func (f *StreamFilter) checkWindowBlock(windowStr string) bool {
	if len(f.blockPatterns) == 0 || f.windowLen == 0 {
		return false
	}
	for _, re := range f.blockPatterns {
		if re.MatchString(windowStr) {
			f.blocked = true
			f.blockReason = re.String()
			return true
		}
	}
	return false
}

func (f *StreamFilter) checkWindowRedact(windowStr string) bool {
	if len(f.secretPatterns) == 0 || f.windowLen == 0 {
		return false
	}
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

// ExtractFinishReasonFromMap extracts the finish_reason from choices[0] of an
// OpenAI-format SSE chunk. Returns "" when the finish_reason is absent, null,
// or not a string. Used to determine whether a no-[DONE] stream ending is
// benign truncation (finish_reason seen) or a genuine mid-generation cut.
func ExtractFinishReasonFromMap(chunk map[string]interface{}) string {
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return ""
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return ""
	}
	fr, _ := choice["finish_reason"].(string)
	return fr
}

func ReplaceDeltaContent(data []byte, newContent string) []byte {
	chunk := ParseSSEChunk(data)
	if chunk == nil {
		return data
	}
	return ReplaceDeltaContentMap(chunk, newContent)
}

// ExtractThinkingSignal detects whether an SSE event indicates active or
// inactive thinking/reasoning phases. Returns (active, hasSignal) where
// hasSignal indicates a definitive state transition (avoid false positives
// from non-thinking events like pings or tool_calls). Handles OpenAI-format
// reasoning_content deltas and Anthropic-format content_block events.
func ExtractThinkingSignal(chunk map[string]interface{}) (active bool, hasSignal bool) {
	if chunk == nil {
		return false, false
	}

	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return checkAnthropicThinking(chunk)
	}

	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return checkAnthropicThinking(chunk)
	}

	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return checkAnthropicThinking(chunk)
	}

	reasoningContent, hasRC := delta["reasoning_content"].(string)
	if hasRC && reasoningContent != "" {
		return true, true
	}
	if hasRC && reasoningContent == "" {
		return false, true
	}

	content, hasContent := delta["content"].(string)
	if hasContent && content != "" {
		return false, true
	}

	return checkAnthropicThinking(chunk)
}

// checkAnthropicThinking checks Anthropic-format events for thinking signals.
// Recognizes content_block_start with type "thinking" and content_block_delta
// with delta type "thinking_delta" as active thinking.
func checkAnthropicThinking(chunk map[string]interface{}) (active bool, hasSignal bool) {
	eventType, ok := chunk["type"].(string)
	if !ok {
		return false, false
	}

	switch eventType {
	case "content_block_start":
		return checkContentBlockStart(chunk)
	case "content_block_delta":
		delta, ok := chunk["delta"].(map[string]interface{})
		if !ok {
			return false, false
		}
		deltaType, ok := delta["type"].(string)
		if !ok {
			return false, false
		}
		if deltaType == "thinking_delta" {
			return true, true
		}
		return false, false
	}
	return false, false
}

// checkContentBlockStart extracts thinking state from Anthropic
// content_block_start events by examining the content_block type field.
func checkContentBlockStart(chunk map[string]interface{}) (active bool, hasSignal bool) {
	cb, ok := chunk["content_block"].(map[string]interface{})
	if !ok {
		return false, false
	}
	cbType, ok := cb["type"].(string)
	if !ok {
		return false, false
	}
	if cbType == "thinking" {
		return true, true
	}
	// Explicit non-thinking type ("text", "tool_use", etc.) signals thinking ended.
	// Empty string is treated as "no signal" to avoid false negatives on malformed events.
	if cbType != "thinking" && cbType != "" {
		return false, true
	}
	return false, false
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
