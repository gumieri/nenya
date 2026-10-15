package stream

import "strings"

// IsStreamErrorPayload reports whether a parsed SSE data payload represents an
// upstream error event rather than a content chunk. It recognizes:
//   - OpenAI-style payloads with a top-level "error" key;
//   - Anthropic-style payloads with "type":"error";
//   - flat payloads whose top-level "type" ends with "_error" (e.g.
//     "server_error", "overloaded_error", "api_error").
//
// Content chunks (choices/candidates/message_*/content_block_* deltas) never
// match these shapes, so they are never misclassified.
func IsStreamErrorPayload(parsed map[string]any) bool {
	if parsed == nil {
		return false
	}
	if v, hasError := parsed["error"]; hasError && v != nil {
		return true
	}
	if t, ok := parsed["type"]; ok {
		if s, ok := t.(string); ok {
			if s == "error" || strings.HasSuffix(s, "_error") {
				return true
			}
		}
	}
	return false
}

// IsResponseFailedEvent reports whether a parsed payload is the OpenAI Responses
// API's terminal failure event. Shared by the bootstrap head probe and the
// buffered MCP replay path (NENYA-153), so both treat the same frame as a
// rejection.
func IsResponseFailedEvent(parsed map[string]any) bool {
	if parsed == nil {
		return false
	}
	t, _ := parsed["type"].(string)
	return t == "response.failed"
}
