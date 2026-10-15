package stream

import (
	"strings"

	"github.com/nenya/internal/util"
)

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

// rejectionText concatenates the fields an upstream uses to report a failure —
// message, type and code, at the top level or nested under "error". Content
// fields (choices, candidates, delta, text) are never inspected: a model that
// quotes "the server is overloaded" in its answer is not a rejection.
func rejectionText(parsed map[string]any) string {
	var b strings.Builder
	appendFields := func(m map[string]any) {
		// "detail" covers FastAPI-style rejections.
		for _, key := range []string{"message", "type", "code", "detail"} {
			if s, ok := m[key].(string); ok {
				b.WriteString(s)
				b.WriteByte(' ')
			}
		}
	}
	appendFields(parsed)
	if errObj, ok := parsed["error"].(map[string]any); ok {
		appendFields(errObj)
	}
	return b.String()
}

// IsCapacityRejection reports whether a parsed upstream payload reports a
// capacity/overload failure that is not shaped as an error envelope
// (NENYA-168) — e.g. {"message":"model is at capacity"} with no "error" key and
// no "_error" type. It shares the overload phrase list with the circuit
// breaker's capacity class, so a rejection the breaker treats as overload also
// releases the NENYA-45 bootstrap buffer for failover.
func IsCapacityRejection(parsed map[string]any) bool {
	if parsed == nil {
		return false
	}
	return util.IsOverloadMessage(rejectionText(parsed))
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
