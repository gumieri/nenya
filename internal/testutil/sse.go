package testutil

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"nenya/internal/stream"
)

// SSECollector is a test double implementing stream.SSEObserver.
// It collects all SSE events for later inspection in tests.
type SSECollector struct {
	Events   []stream.SSEEvent
	Closed   bool
	CloseErr error
	mu       sync.Mutex
}

func (c *SSECollector) OnSSEEvent(event stream.SSEEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Events = append(c.Events, event)
}

func (c *SSECollector) OnStreamClose(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Closed = true
	c.CloseErr = err
}

// EventCount returns the number of collected events.
func (c *SSECollector) EventCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.Events)
}

// EventsByType returns all events of the given type.
func (c *SSECollector) EventsByType(typ string) []stream.SSEEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []stream.SSEEvent
	for _, e := range c.Events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// Content returns the concatenated content from all "content" events.
func (c *SSECollector) Content() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, e := range c.Events {
		if e.Type == "content" {
			if data, ok := e.Data["content"].(string); ok {
				b.WriteString(data)
			}
		}
	}
	return b.String()
}

// HasContent returns true if any content event contains the given substring.
func (c *SSECollector) HasContent(s string) bool {
	return strings.Contains(c.Content(), s)
}

// UsageEvents returns all events with usage data.
func (c *SSECollector) UsageEvents() []stream.SSEEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []stream.SSEEvent
	for _, e := range c.Events {
		if e.Type == "usage" {
			out = append(out, e)
		}
	}
	return out
}

// LastEvent returns the last collected event, or nil if empty.
func (c *SSECollector) LastEvent() *stream.SSEEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.Events) == 0 {
		return nil
	}
	cp := c.Events[len(c.Events)-1]
	return &cp
}

// Reset clears all collected state.
func (c *SSECollector) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Events = c.Events[:0]
	c.Closed = false
	c.CloseErr = nil
}

// ParseSSEEvents parses raw SSE bytes into structured events.
// Useful for validating SSE output in tests without fragile string matching.
func ParseSSEEvents(t *testing.T, raw []byte) []stream.SSEEvent {
	t.Helper()

	var events []stream.SSEEvent
	lines := strings.Split(string(raw), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				events = append(events, stream.SSEEvent{Type: "done", Raw: []byte(line)})
				continue
			}

			var parsed map[string]interface{}
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				events = append(events, stream.SSEEvent{Type: "data", Raw: []byte(line)})
				continue
			}

			typ := classifyEvent(parsed)
			events = append(events, stream.SSEEvent{
				ID:   extractID(parsed),
				Type: typ,
				Data: parsed,
				Raw:  []byte(line),
			})
		}
	}

	return events
}

// ExtractSSEContent extracts all delta content from SSE chunks.
// Returns the concatenated content string.
func ExtractSSEContent(t *testing.T, raw []byte) string {
	t.Helper()

	events := ParseSSEEvents(t, raw)
	var b strings.Builder
	for _, e := range events {
		if content, ok := extractContent(e.Data); ok {
			b.WriteString(content)
		}
	}
	return b.String()
}

// ParseChatStream parses an OpenAI-format SSE stream into chunk maps.
// Returns the chunks and whether [DONE] was seen.
func ParseChatStream(t *testing.T, raw []byte) ([]map[string]interface{}, bool) {
	t.Helper()

	events := ParseSSEEvents(t, raw)
	chunks := make([]map[string]interface{}, 0, len(events))
	done := false

	for _, e := range events {
		if e.Type == "done" {
			done = true
			continue
		}
		if e.Data == nil {
			continue
		}
		chunks = append(chunks, e.Data)
	}

	return chunks, done
}

func classifyEvent(data map[string]interface{}) string {
	if _, ok := data["usage"]; ok {
		return "usage"
	}
	if choices, ok := data["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			return classifyDeltaEvent(choice)
		}
	}
	if _, ok := data["error"]; ok {
		return "error"
	}
	return "data"
}

func classifyDeltaEvent(choice map[string]interface{}) string {
	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return "chunk"
	}
	if _, ok := delta["tool_calls"]; ok {
		return "tool_call"
	}
	if _, ok := delta["content"].(string); ok {
		return "content"
	}
	if _, ok := delta["reasoning"].(string); ok {
		return "reasoning"
	}
	return "chunk"
}

func extractID(data map[string]interface{}) string {
	if id, ok := data["id"].(string); ok {
		return id
	}
	return ""
}

func extractContent(data map[string]interface{}) (string, bool) {
	if data == nil {
		return "", false
	}
	choices, ok := data["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return "", false
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return "", false
	}
	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return "", false
	}
	content, ok := delta["content"].(string)
	return content, ok
}

// BytesToString is a test helper that converts []byte to string.
// Useful for comparing SSE output in table-driven tests.
func BytesToString(b []byte) string {
	return string(b)
}

// ContainsAny returns true if haystack contains any of the needles.
func ContainsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
