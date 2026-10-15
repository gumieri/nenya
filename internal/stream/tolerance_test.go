package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
)

// NENYA-197 — SSE parser tolerance for non-canonical framing, and malformed-
// stream robustness. Upstream bytes are untrusted input to the SSE
// transformers: a parser that silently drops a field it does not recognize
// loses content (and skips the security filters that run over parsed payloads),
// and a transform path that panics or allocates without bound is a
// denial-of-service surface.

// TestSSEDataField pins the field-recognition grammar, including the
// tolerance NENYA-197 adds: a data field is "data:" plus an OPTIONAL single
// space. Real providers emit "data:{json}"; a strict parser dropped the whole
// field, so the client received a stream with no content and the stream filters
// never saw the payload.
func TestSSEDataField(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		want   string
		wantOK bool
	}{
		{name: "canonical", line: "data: {\"a\":1}", want: "{\"a\":1}", wantOK: true},
		{name: "no space", line: "data:{\"a\":1}", want: "{\"a\":1}", wantOK: true},
		{name: "two spaces keeps one", line: "data:  {\"a\":1}", want: " {\"a\":1}", wantOK: true},
		{name: "empty value", line: "data:", want: "", wantOK: true},
		{name: "empty value with space", line: "data: ", want: "", wantOK: true},
		{name: "done no space", line: "data:[DONE]", want: "[DONE]", wantOK: true},
		{name: "done canonical", line: "data: [DONE]", want: "[DONE]", wantOK: true},
		{name: "event field is not data", line: "event: message", wantOK: false},
		{name: "comment is not data", line: ": keepalive", wantOK: false},
		{name: "bare json is not a data field", line: "{\"a\":1}", wantOK: false},
		{name: "no colon", line: "data", wantOK: false},
		{name: "uppercase is not data", line: "DATA: {\"a\":1}", wantOK: false},
		{name: "tab after colon is part of the value", line: "data:\t{\"a\":1}", want: "\t{\"a\":1}", wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := SSEDataField([]byte(tt.line))
			if ok != tt.wantOK {
				t.Fatalf("SSEDataField(%q) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
			if ok && string(got) != tt.want {
				t.Fatalf("SSEDataField(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

// TestSSETransformingReader_NonCanonicalDataFieldIsProcessed pins the
// consequence of the tolerance: a no-space data line reaches the transformer and
// the stream filters (it used to be treated as a non-SSE line and passed through
// untouched), and a no-space [DONE] is recognized so the stream ends cleanly
// instead of being sanitized with an injected gateway_error.
func TestSSETransformingReader_NonCanonicalDataFieldIsProcessed(t *testing.T) {
	t.Run("no-space payload reaches the transformer", func(t *testing.T) {
		input := "data:{\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata:[DONE]\n\n"
		out := framingReader(t, input, 0, reencodeTransformer{})
		if !strings.Contains(out, `"seen":true`) {
			t.Fatalf("the transformer did not see the no-space payload: %q", out)
		}
		if strings.Contains(out, "gateway_error") {
			t.Fatalf("a no-space [DONE] must be recognized: %q", out)
		}
		assertWellFormedEvents(t, out, 2)
	})

	t.Run("no-space payload is scanned by the stream filters", func(t *testing.T) {
		canary := "NENYA-CANARY-0123456789abcdef0123456789abcdef"
		input := "data:{\"choices\":[{\"delta\":{\"content\":\"" + canary + "\"}}]}\n\n"
		r := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
		r.SetCanaryWatcher(NewCanaryWatcher(canary, "block", "stream", nil, nil))

		var out bytes.Buffer
		if _, err := io.Copy(&out, r); !errors.Is(err, ErrCanaryDetected) {
			t.Fatalf("err = %v, want ErrCanaryDetected (the no-space payload must be scanned)", err)
		}
	})
}

// TestNormalizeToolCalls_MalformedIndices pins the index guard: an upstream
// index that is negative, non-numeric, fractional or astronomically large must
// fall back to the array position instead of panicking or colliding all calls
// onto index 0.
func TestNormalizeToolCalls_MalformedIndices(t *testing.T) {
	tests := []struct {
		name  string
		index any
	}{
		{name: "negative", index: float64(-1)},
		{name: "non-numeric string", index: "abc"},
		{name: "empty string", index: ""},
		{name: "null", index: nil},
		{name: "fractional", index: 1.5},
		{name: "huge float", index: 1e300},
		{name: "object", index: map[string]any{"a": 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := newToolCallState()
			chunk := map[string]any{
				"choices": []any{map[string]any{
					"delta": map[string]any{
						"tool_calls": []any{
							map[string]any{"index": tt.index, "id": "call_1", "function": map[string]any{"name": "f", "arguments": "{}"}},
							map[string]any{"index": tt.index, "id": "call_2", "function": map[string]any{"name": "g", "arguments": "{}"}},
						},
					},
				}},
			}
			// Must not panic, and the two calls must land on distinct indices
			// (the array position fallback), not both on 0.
			normalizeToolCalls(chunk, &state)
			if len(state.seenIndices) != 2 {
				t.Fatalf("seenIndices = %v, want two distinct indices (fallback to array position)", state.seenIndices)
			}
			if _, err := json.Marshal(chunk); err != nil {
				t.Fatalf("chunk must stay marshalable: %v", err)
			}
		})
	}
}

// TestSSETransformingReader_MalformedPayloadsArePanicFree pins the robustness of
// the transform path for hostile payload shapes: wrong types at every level,
// absurd indices and truncated JSON must not panic and must leave the reader
// able to finish the stream.
func TestSSETransformingReader_MalformedPayloadsArePanicFree(t *testing.T) {
	payloads := []string{
		`{"choices":"not-an-array"}`,
		`{"choices":[{"delta":42}]}`,
		`{"choices":[{"delta":{"tool_calls":"nope"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":-5,"function":7}]}}]}`,
		`{"choices":[{"delta":{"content":123}}]}`,
		`{"choices":[{"delta":{"content":"ok"},"finish_reason":[]}]}`,
		`[1,2,3]`,
		`{"partial`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1e400,"id":null}]}}]}`,
	}
	for _, payload := range payloads {
		t.Run(payload, func(t *testing.T) {
			// Both spellings of the data field: the hostile bytes must reach the
			// parser either way, and the terminal must still be recognized (with
			// a strict parser a no-space [DONE] is not, so the reader injects a
			// spurious gateway_error).
			for _, frame := range []string{"data: ", "data:"} {
				input := frame + payload + "\n\n" + frame + "[DONE]\n\n"
				for _, tf := range []ResponseTransformer{nil, reencodeTransformer{}} {
					r := NewSSETransformingReader(strings.NewReader(input), tf, context.Background())
					var out bytes.Buffer
					if _, err := io.Copy(&out, r); err != nil && err != io.EOF {
						t.Fatalf("copy (%s): %v", frame, err)
					}
					if !r.SawDone() {
						t.Fatalf("terminal not recognized for frame %q: %q", frame, out.String())
					}
					if r.InjectedError() {
						t.Fatalf("a completed stream was sanitized for frame %q: %q", frame, out.String())
					}
					if _, unterminated := parseSSEEvents(t, out.String()); unterminated {
						t.Fatalf("stream left pending: %q", out.String())
					}
				}
			}
		})
	}
}

// TestSSETransformingReader_MixedLineEndings pins that a stream mixing LF, CRLF
// and lone CR line endings is parsed per line and normalized to LF, with no
// field dropped and no event glued. A line terminator is not an event
// terminator: two data lines with no blank line between them are ONE event with
// two data lines (which is how SSE carries a multi-line payload), so the
// fixtures cover both shapes.
func TestSSETransformingReader_MixedLineEndings(t *testing.T) {
	t.Run("each line is its own event", func(t *testing.T) {
		input := "data: {\"a\":1}\r\n\r\ndata: {\"b\":2}\r\rdata: {\"c\":3}\n\ndata: [DONE]\r\n\r\n"
		out := framingReader(t, input, 0, nil)
		events, unterminated := parseSSEEvents(t, out)
		if unterminated {
			t.Fatalf("stream ended pending: %q", out)
		}
		if len(events) != 4 {
			t.Fatalf("got %d events, want 4: %q", len(events), out)
		}
		for i, ev := range events {
			if len(ev.Data) != 1 {
				t.Fatalf("event %d has %d data lines: %q", i, len(ev.Data), out)
			}
		}
		if events[3].Data[0] != "[DONE]" {
			t.Fatalf("final event = %q, want [DONE]: %q", events[3].Data[0], out)
		}
		if strings.Contains(out, "\r") {
			t.Fatalf("output must be LF-normalized: %q", out)
		}
	})

	t.Run("consecutive data lines stay one event", func(t *testing.T) {
		// CRLF after the first line is a line terminator, not a blank line.
		input := "data: {\"a\":1}\r\ndata: {\"b\":2}\r\rdata: [DONE]\n\n"
		out := framingReader(t, input, 0, nil)
		events, unterminated := parseSSEEvents(t, out)
		if unterminated {
			t.Fatalf("stream ended pending: %q", out)
		}
		if len(events) != 2 {
			t.Fatalf("got %d events, want 2 (the multi-line event + [DONE]): %q", len(events), out)
		}
		if len(events[0].Data) != 2 {
			t.Fatalf("event 0 has %d data lines, want 2: %q", len(events[0].Data), out)
		}
	})
}

// TestSSETransformingReader_TerminalMatrixWithNoSpaceField pins that the
// tolerance reaches the terminal-state matrix (NENYA-197): a no-space data line
// must register content and finish_reason, or a stream that completed is
// misclassified — a spurious gateway_error is injected, the synthetic
// finish_reason is skipped (the NENYA-46 hang), continuation is disabled and the
// response cache is blocked.
func TestSSETransformingReader_TerminalMatrixWithNoSpaceField(t *testing.T) {
	t.Run("content and finish_reason are registered", func(t *testing.T) {
		input := "data:{\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\ndata:[DONE]\n\n"
		r := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
		var out bytes.Buffer
		if _, err := io.Copy(&out, r); err != nil && err != io.EOF {
			t.Fatalf("copy: %v", err)
		}
		if !r.SawContent() {
			t.Fatal("SawContent() = false for a no-space content frame")
		}
		if !r.SawFinishReason() {
			t.Fatal("SawFinishReason() = false for a no-space finish_reason")
		}
		if !r.SawDone() {
			t.Fatal("SawDone() = false for a no-space [DONE]")
		}
		if r.InjectedError() {
			t.Fatal("a completed no-space stream must not be sanitized with an injected error")
		}
		if strings.Contains(out.String(), "gateway_error") {
			t.Fatalf("spurious gateway_error injected: %q", out.String())
		}
	})

	t.Run("synthetic finish_reason is still emitted before a no-space [DONE]", func(t *testing.T) {
		input := "data:{\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\ndata:[DONE]\n\n"
		out := framingReader(t, input, 0, nil)
		if !strings.Contains(out, `"finish_reason":"stop"`) {
			t.Fatalf("the synthetic finish_reason is missing (NENYA-46 hang): %q", out)
		}
	})
}

// TestSSETransformingReader_NonCanonicalFieldChunkSplits pins that a no-space
// data line is split-tolerant: the payload is processed identically however the
// read chunks fall, including splits inside the "data:" field name.
func TestSSETransformingReader_NonCanonicalFieldChunkSplits(t *testing.T) {
	inputs := map[string]string{
		"no space":       "data:{\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\ndata:[DONE]\n\n",
		"mixed spacing":  "data:{\"a\":1}\n\ndata: {\"b\":2}\n\ndata:[DONE]\n\n",
		"cr only":        "data:{\"a\":1}\r\rdata:[DONE]\r\r",
		"cr at the edge": "data:{\"a\":1}\rdata:[DONE]\r\r",
	}
	for name, input := range inputs {
		t.Run(name, func(t *testing.T) {
			base := framingReader(t, input, 0, nil)
			if _, unterminated := parseSSEEvents(t, base); unterminated {
				t.Fatalf("base output pending: %q", base)
			}
			for size := 1; size <= len(input); size++ {
				got := framingReader(t, input, size, nil)
				if got != base {
					t.Fatalf("chunk size %d changed the output:\nbase=%q\n got=%q", size, base, got)
				}
			}
		})
	}
}

// TestSSEDataField_LongerFieldNamesAreNotData pins that the prefix match cannot
// swallow a longer field name.
func TestSSEDataField_LongerFieldNamesAreNotData(t *testing.T) {
	for _, line := range []string{"database: x", "data_extra: x", "data : x", "dat: x"} {
		if value, ok := SSEDataField([]byte(line)); ok {
			t.Errorf("SSEDataField(%q) = %q, true; want not-a-data-field", line, value)
		}
	}
}

// TestSSETransformingReader_RedactFilterSeesNoSpaceField pins that a no-space
// data line reaches a filter that REWRITES (not blocks) the payload, so a
// stripped value is not silently passed through.
func TestSSETransformingReader_RedactFilterSeesNoSpaceField(t *testing.T) {
	secret := "AKIAIOSFODNN7EXAMPLE"
	input := "data:{\"choices\":[{\"delta\":{\"content\":\"" + secret + "\"}}]}\n\ndata:[DONE]\n\n"
	pattern := regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	r := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
	r.SetStreamFilter(NewStreamFilter([]*regexp.Regexp{pattern}, nil, "[REDACTED]", 256))

	var out bytes.Buffer
	if _, err := io.Copy(&out, r); err != nil && err != io.EOF {
		t.Fatalf("copy: %v", err)
	}
	if strings.Contains(out.String(), secret) {
		t.Fatalf("the redact filter did not see the no-space payload: %q", out.String())
	}
	if !strings.Contains(out.String(), "[REDACTED]") {
		t.Fatalf("expected a redaction placeholder: %q", out.String())
	}
}

// TestSSETransformingReader_EmptyDataValueAtEOF pins a bare data field at EOF:
// it is a keepalive, not content, and the stream still ends terminated.
func TestSSETransformingReader_EmptyDataValueAtEOF(t *testing.T) {
	out := framingReader(t, "data: {\"a\":1}\n\ndata:\n", 0, nil)
	if !strings.HasSuffix(out, "\n\n") {
		t.Fatalf("stream not terminated: %q", out)
	}
	events, unterminated := parseSSEEvents(t, out)
	if unterminated {
		t.Fatalf("stream ended pending: %q", out)
	}
	if events[len(events)-1].Data[0] != "[DONE]" {
		t.Fatalf("final event = %q, want [DONE]: %q", events[len(events)-1].Data[0], out)
	}
	if !strings.Contains(out, "gateway_error") {
		t.Fatalf("the keepalive-then-EOF cut must be sanitized: %q", out)
	}
}

// TestSSETransformingReader_NonCanonicalDoneSpelling pins that a terminal
// spelled non-canonically is recognized AND forwarded canonically: the reader
// must not pass a spelling a strict client would ignore as the only sentinel
// (the tolerance premise is that strict parsers exist).
func TestSSETransformingReader_NonCanonicalDoneSpelling(t *testing.T) {
	for _, done := range []string{"data:[DONE]", "data:  [DONE]", "data:\t[DONE]", "data: [DONE] ", "data: [DONE]"} {
		t.Run(done, func(t *testing.T) {
			input := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n" + done + "\n\n"
			r := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
			var out bytes.Buffer
			if _, err := io.Copy(&out, r); err != nil && err != io.EOF {
				t.Fatalf("copy: %v", err)
			}
			if !r.SawDone() {
				t.Fatalf("SawDone() = false for %q", done)
			}
			if r.InjectedError() {
				t.Fatalf("a completed stream was sanitized for %q: %q", done, out.String())
			}
			if !strings.HasSuffix(out.String(), "data: [DONE]\n\n") {
				t.Fatalf("the canonical sentinel is not the last event for %q: %q", done, out.String())
			}
		})
	}
}
