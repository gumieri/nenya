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
	if _, hasError := parsed["error"]; hasError {
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
