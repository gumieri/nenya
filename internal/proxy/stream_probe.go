package proxy

import (
	"bytes"
	"encoding/json"
	"sync"

	"github.com/nenya/internal/stream"
)

// dataPrefixLen is the length of the "data:" SSE field prefix.
const dataPrefixLen = len("data:")

// jsonMapPool recycles the map[string]any used for probing SSE payloads; the
// probe runs once per streaming request but avoids per-line allocations.
var jsonMapPool = sync.Pool{
	New: func() any { return map[string]any{} },
}

// streamHeadKind classifies the first chunk read from an upstream SSE stream
// before any headers are committed to the client.
type streamHeadKind int

const (
	// headUndetermined means the chunk contained no complete recognizable
	// event (e.g. partial JSON payloads) — the stream is streamed normally.
	headUndetermined streamHeadKind = iota
	// headNormal means the chunk began with a content-bearing or neutral event
	// (or the [DONE] marker) — no early upstream error.
	headNormal
	// headError means the very first meaningful event in the chunk is an
	// upstream error payload, delivered before any content.
	headError
)

// classifyStreamHead inspects the first chunk of an upstream SSE stream and
// reports whether it opened with an upstream error event. Only complete,
// recognized event lines are considered: JSON payloads are matched against
// stream.IsStreamErrorPayload; [DONE], keep-alives, comments, partial JSON and
// unknown data are treated as headNormal/headUndetermined so a stream that may
// carry content is never misclassified.
func classifyStreamHead(chunk []byte) streamHeadKind {
	for _, raw := range bytes.Split(chunk, []byte{'\n'}) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		if sseFieldLine(line) {
			continue
		}
		data, isData := sseDataLine(line)
		if !isData || len(data) == 0 {
			continue
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			return headNormal
		}
		parsed := jsonMapPool.Get().(map[string]any)
		// json.Unmarshal only overwrites keys present in the new payload, so a
		// reused map can otherwise carry stale keys (e.g. an "error" from a
		// previous probe) that would corrupt classification — clear first.
		clear(parsed)
		err := json.Unmarshal(data, &parsed)
		if err != nil {
			clear(parsed)
			jsonMapPool.Put(parsed)
			// Incomplete/partial JSON: cannot decide, leave undetermined.
			return headUndetermined
		}
		isErr := stream.IsStreamErrorPayload(parsed)
		clear(parsed)
		jsonMapPool.Put(parsed)
		if isErr {
			return headError
		}
		// The first complete, meaningful payload is not an upstream error:
		// treat the head as content-bearing so we never fail over past client
		// bytes.
		return headNormal
	}
	return headUndetermined
}

// sseFieldLine reports whether line is an SSE field line (event:/id:/retry:)
// that carries no event payload of its own.
func sseFieldLine(line []byte) bool {
	for _, prefix := range [][]byte{[]byte("event:"), []byte("id:"), []byte("retry:")} {
		if bytes.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// sseDataLine extracts the payload of a data: line, or a bare JSON line (some
// upstreams emit raw JSON bodies under Content-Type: text/event-stream).
func sseDataLine(line []byte) ([]byte, bool) {
	if bytes.HasPrefix(line, []byte("data:")) {
		return bytes.TrimSpace(line[dataPrefixLen:]), true
	}
	if line[0] == '{' {
		return line, true
	}
	return nil, false
}

// bootstrapVerdict is the decision a scanned bootstrap event reached:
// undetermined events keep the buffer open, an output event flushes it, and a
// rejection fails the target over before the 200 is committed (NENYA-45).
type bootstrapVerdict int

const (
	// bootstrapUndetermined: handshake/metadata/unknown event — keep buffering.
	bootstrapUndetermined bootstrapVerdict = iota
	// bootstrapOutput: an allow-listed real-output event — commit and flush.
	bootstrapOutput
	// bootstrapReject: an in-stream rejection payload — failover candidate.
	bootstrapReject
)

// classifyBootstrapLine classifies one complete SSE data payload for the
// bootstrap buffer. Rejections use stream.IsStreamErrorPayload plus the
// OpenAI Responses "response.failed" event. Output detection is an
// event-type allow-list — NOT a frame count — because providers emit
// rate-limit/metadata frames before the response starts: only events that
// provably carry generation output release the buffer. Everything else
// (handshakes, pings, usage frames, partial JSON, unknown shapes) is
// undetermined and keeps the buffer open; the byte budget bounds how long
// that can last.
func classifyBootstrapLine(data []byte) bootstrapVerdict {
	if bytes.Equal(data, []byte("[DONE]")) {
		return bootstrapUndetermined
	}
	parsed := jsonMapPool.Get().(map[string]any)
	clear(parsed)
	defer clear(parsed)
	defer jsonMapPool.Put(parsed)
	if err := json.Unmarshal(data, &parsed); err != nil {
		// Partial or non-JSON payload: cannot decide — keep buffering.
		return bootstrapUndetermined
	}
	if stream.IsStreamErrorPayload(parsed) {
		return bootstrapReject
	}
	if t, _ := parsed["type"].(string); t == "response.failed" {
		return bootstrapReject
	}
	return classifyBootstrapOutput(parsed)
}

// classifyBootstrapOutput applies the real-output allow-list to a parsed SSE
// payload: OpenAI chat chunks with content/tool_calls deltas, Anthropic
// content-block events, and Gemini candidates carrying parts.
func classifyBootstrapOutput(parsed map[string]any) bootstrapVerdict {
	// Anthropic: message_start/ping/message_delta/message_stop are metadata;
	// content_block_* events carry generation output.
	switch t, _ := parsed["type"].(string); t {
	case "content_block_start", "content_block_delta", "content_block_stop":
		return bootstrapOutput
	}
	// OpenAI chat-completions chunks (delta) and non-streaming shapes (message).
	if choices, ok := parsed["choices"].([]any); ok {
		return openAIChoiceHasOutput(choices)
	}
	// Gemini SSE candidates: content.parts carrying text/functionCall.
	if cands, ok := parsed["candidates"].([]any); ok {
		return geminiCandidateHasOutput(cands)
	}
	return bootstrapUndetermined
}

// openAIChoiceHasOutput reports whether the first OpenAI-style choice carries
// generation output in its delta or message object.
func openAIChoiceHasOutput(choices []any) bootstrapVerdict {
	if len(choices) == 0 {
		return bootstrapUndetermined
	}
	c, ok := choices[0].(map[string]any)
	if !ok {
		return bootstrapUndetermined
	}
	if delta, ok := c["delta"].(map[string]any); ok && bootstrapHasContent(delta) {
		return bootstrapOutput
	}
	if msg, ok := c["message"].(map[string]any); ok && bootstrapHasContent(msg) {
		return bootstrapOutput
	}
	return bootstrapUndetermined
}

// geminiCandidateHasOutput reports whether the first Gemini candidate carries
// content parts.
func geminiCandidateHasOutput(cands []any) bootstrapVerdict {
	if len(cands) == 0 {
		return bootstrapUndetermined
	}
	c, ok := cands[0].(map[string]any)
	if !ok {
		return bootstrapUndetermined
	}
	content, ok := c["content"].(map[string]any)
	if !ok {
		return bootstrapUndetermined
	}
	if parts, ok := content["parts"].([]any); ok && len(parts) > 0 {
		return bootstrapOutput
	}
	return bootstrapUndetermined
}

// bootstrapHasContent reports whether an OpenAI-style delta/message object
// carries actual generation output (text, tool calls, or reasoning text).
// A role-only first chunk is a handshake, not output.
func bootstrapHasContent(m map[string]any) bool {
	if s, ok := m["content"].(string); ok && s != "" {
		return true
	}
	if tc, ok := m["tool_calls"].([]any); ok && len(tc) > 0 {
		return true
	}
	if rc, ok := m["reasoning_content"].(string); ok && rc != "" {
		return true
	}
	return false
}

// scanBootstrapBuffer walks the complete lines of acc starting at offset and
// returns the first decisive bootstrap verdict together with the offset just
// past the last complete line scanned. The trailing incomplete line (no
// terminating '\n' yet) is left unscanned for the next read.
func scanBootstrapBuffer(acc []byte, offset int) (bootstrapVerdict, int) {
	for offset < len(acc) {
		idx := bytes.IndexByte(acc[offset:], '\n')
		if idx < 0 {
			break
		}
		line := bytes.TrimSpace(acc[offset : offset+idx])
		offset += idx + 1
		if len(line) == 0 || line[0] == ':' || sseFieldLine(line) {
			continue
		}
		data, isData := sseDataLine(line)
		if !isData || len(data) == 0 {
			continue
		}
		if v := classifyBootstrapLine(data); v != bootstrapUndetermined {
			return v, offset
		}
	}
	return bootstrapUndetermined, offset
}
