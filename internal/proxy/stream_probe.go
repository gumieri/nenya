package proxy

import (
	"bytes"
	"encoding/json"
	"sync"

	"github.com/nenya/internal/stream"
)

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

// forEachSSELine walks the SSE lines of buf starting at offset, calling fn with
// each line (terminator removed). fn returns false to stop the walk. It returns
// the offset just past the last line consumed. With atEOF false a trailing
// incomplete line — or a CR at the buffer edge — is left unscanned for the next
// read (the incremental bootstrap buffer); with atEOF true the trailing segment
// is treated as a complete line, because a one-shot probe has no next read (an
// upstream error frame that arrived without a terminator must still be
// classified, or NENYA-45 early failover is lost).
// The splitter is the shared one (LF, CRLF or lone CR), so the probe agrees with
// the transforming reader on non-canonical framing (NENYA-197).
func forEachSSELine(buf []byte, offset int, atEOF bool, fn func(line []byte) bool) int {
	for offset < len(buf) {
		advance, token, _ := stream.SplitSSELines(buf[offset:], atEOF)
		if advance == 0 {
			break
		}
		offset += advance
		if !fn(token) {
			break
		}
	}
	return offset
}

// classifyStreamHead inspects the first chunk of an upstream SSE stream and
// reports whether it opened with an upstream error event. Only complete,
// recognized event lines are considered: JSON payloads are matched against
// stream.IsStreamErrorPayload; [DONE], keep-alives, comments, partial JSON and
// unknown data are treated as headNormal/headUndetermined so a stream that may
// carry content is never misclassified.
func classifyStreamHead(chunk []byte) streamHeadKind {
	verdict := headUndetermined
	forEachSSELine(chunk, 0, true, func(raw []byte) bool {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 || line[0] == ':' {
			return true
		}
		if sseFieldLine(line) {
			return true
		}
		data, isData := sseDataLine(line)
		if !isData || len(data) == 0 {
			return true
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			verdict = headNormal
			return false
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
			verdict = headUndetermined
			return false
		}
		isErr := stream.IsStreamErrorPayload(parsed)
		clear(parsed)
		jsonMapPool.Put(parsed)
		if isErr {
			verdict = headError
			return false
		}
		// The first complete, meaningful payload is not an upstream error:
		// treat the head as content-bearing so we never fail over past client
		// bytes.
		verdict = headNormal
		return false
	})
	return verdict
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
	if value, isData := stream.SSEDataField(line); isData {
		return bytes.TrimSpace(value), true
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
	// bootstrapOutput: a frame carrying effective output — commit and flush.
	bootstrapOutput
	// bootstrapReject: an in-stream rejection payload — failover candidate.
	bootstrapReject
)

// classifyBootstrapLine classifies one complete SSE data payload for the
// bootstrap buffer. Rejections use stream.IsStreamErrorPayload plus the
// OpenAI Responses "response.failed" event. Output detection is a
// content-based effective-output test (stream.HasEffectiveOutput, NENYA-178) —
// NOT a frame count — because providers emit rate-limit/metadata frames before
// the response starts: only frames that provably carry generation output release
// the buffer. Everything else (handshakes, pings, usage frames, partial JSON,
// unknown shapes) is undetermined and keeps the buffer open; the byte budget
// bounds how long that can last.
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

// classifyBootstrapOutput applies the real-output test to a parsed SSE payload.
// The rules live in stream.HasEffectiveOutput (NENYA-178) so the bootstrap and
// the TTFT clock cannot drift apart; see its doc for the exact per-format
// classification. The bootstrap opts reasoning frames IN — a reasoning-only
// frame means the provider is generating, not rejecting, so the handshake
// buffer is not held for the whole thinking phase.
func classifyBootstrapOutput(parsed map[string]any) bootstrapVerdict {
	if stream.HasEffectiveOutput(parsed, true) {
		return bootstrapOutput
	}
	return bootstrapUndetermined
}

// scanBootstrapBuffer walks the complete lines of acc starting at offset and
// returns the first decisive bootstrap verdict together with the offset just
// past the last complete line scanned. The trailing incomplete line (no
// terminating '\n' yet) is left unscanned for the next read.
func scanBootstrapBuffer(acc []byte, offset int) (bootstrapVerdict, int) {
	verdict := bootstrapUndetermined
	offset = forEachSSELine(acc, offset, false, func(raw []byte) bool {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 || line[0] == ':' || sseFieldLine(line) {
			return true
		}
		data, isData := sseDataLine(line)
		if !isData || len(data) == 0 {
			return true
		}
		if v := classifyBootstrapLine(data); v != bootstrapUndetermined {
			verdict = v
			return false
		}
		return true
	})
	return verdict, offset
}
