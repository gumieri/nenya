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
