package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/nenya/config"
)

// NENYA-169 — SSE framing edge cases.
//
// Three framing properties are pinned here:
//
//  1. Every stream the reader produces ends with a blank line. SSE dispatches
//     an event on the blank line, and a spec-compliant client DISCARDS a
//     pending event at EOF — so an upstream that omitted the trailing blank
//     line (or a stream that ended on a forwarded frame) would silently lose
//     its last event, including the [DONE] marker, and hang clients that wait
//     for it.
//  2. A frame the gateway synthesizes or injects (synthetic finish_reason,
//     gateway_error, [DONE]) is separated from the previous event by a blank
//     line. Without it the injected data: line joins the previous event's data
//     buffer and the client receives ONE event whose payload is the
//     concatenation — invalid JSON.
//  3. Chunk boundaries are invisible: a CRLF or LF split across read chunks
//     (including a lone CR at a chunk edge) must not change the output bytes.
//
// A well-formed (blank-line terminated) upstream stream must pass through
// byte-identically; the fixes above only repair unterminated framing.

// chunkedReader serves its payload in fixed-size chunks so a test can split a
// stream at an arbitrary byte boundary.
type chunkedReader struct {
	data []byte
	size int
	pos  int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	end := c.pos + c.size
	if end > len(c.data) {
		end = len(c.data)
	}
	n := copy(p, c.data[c.pos:end])
	c.pos += n
	return n, nil
}

// framingReader runs the SSE transforming reader over input, optionally
// delivering it in fixed-size chunks (size 0 = one shot).
func framingReader(t *testing.T, input string, size int, tf ResponseTransformer) string {
	t.Helper()
	var src io.Reader = strings.NewReader(input)
	if size > 0 {
		src = &chunkedReader{data: []byte(input), size: size}
	}
	r := NewSSETransformingReader(src, tf, context.Background())
	var out bytes.Buffer
	if _, err := io.Copy(&out, r); err != nil && err != io.EOF {
		t.Fatalf("copy: %v", err)
	}
	return out.String()
}

// parsedEvent is one SSE event: the data lines collected before its blank-line
// terminator.
type parsedEvent struct {
	Data []string
}

// parseSSEEvents splits an SSE stream into events, collecting data: lines and
// ignoring the other field lines (comments, event:/id:/retry:) that the reader
// forwards verbatim. unterminated reports that the stream ended with a pending
// DATA event — which a spec-compliant client discards (a pending comment/field
// line dispatches nothing, so it is not reported).
func parseSSEEvents(t *testing.T, out string) (events []parsedEvent, unterminated bool) {
	t.Helper()
	lines := strings.Split(out, "\n")
	// The trailing element is the artifact of the final line terminator, not a
	// blank line.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var cur parsedEvent
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			if len(cur.Data) > 0 {
				events = append(events, cur)
				cur = parsedEvent{}
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			cur.Data = append(cur.Data, strings.TrimSpace(rest))
		}
	}
	if len(cur.Data) > 0 {
		events = append(events, cur)
		unterminated = true
	}
	return events, unterminated
}

// assertWellFormedEvents asserts the stream ends terminated and that every
// event carries exactly one data line whose payload is valid JSON or the
// [DONE] sentinel (a glued event would produce a multi-line, non-JSON payload).
func assertWellFormedEvents(t *testing.T, out string, wantEvents int) {
	t.Helper()
	events, unterminated := parseSSEEvents(t, out)
	if unterminated {
		t.Fatalf("stream ended with an unterminated (pending) event: %q", out)
	}
	if len(events) != wantEvents {
		t.Fatalf("got %d SSE events, want %d: %q", len(events), wantEvents, out)
	}
	for i, ev := range events {
		if len(ev.Data) != 1 {
			t.Fatalf("event %d carries %d data lines (frames were glued): %q", i, len(ev.Data), out)
		}
		if ev.Data[0] == "[DONE]" {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(ev.Data[0]), &parsed); err != nil {
			t.Fatalf("event %d payload is not valid JSON (%v): %q", i, err, ev.Data[0])
		}
	}
}

// reencodeTransformer re-serializes a chunk the way a real format transformer
// does (parse, rebuild, emit).
type reencodeTransformer struct{}

func (reencodeTransformer) TransformSSEChunk(_ context.Context, data []byte) ([]byte, error) {
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, err
	}
	parsed["seen"] = true
	return json.Marshal(parsed)
}

func TestSSETransformingReader_FinalEventTerminated(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		transformer ResponseTransformer
		wantEvents  int
	}{
		{
			name:       "lf, blank-line terminated",
			input:      "data: {\"a\":1}\n\ndata: [DONE]\n\n",
			wantEvents: 2,
		},
		{
			name:       "crlf, blank-line terminated",
			input:      "data: {\"a\":1}\r\n\r\ndata: [DONE]\r\n\r\n",
			wantEvents: 2,
		},
		{
			name:       "lf, final frame missing its blank line",
			input:      "data: {\"a\":1}\n\ndata: [DONE]\n",
			wantEvents: 2,
		},
		{
			name:       "crlf, final frame missing its blank line",
			input:      "data: {\"a\":1}\r\n\r\ndata: [DONE]\r\n",
			wantEvents: 2,
		},
		{
			name:       "lf, final frame missing its newline",
			input:      "data: {\"a\":1}\n\ndata: [DONE]",
			wantEvents: 2,
		},
		{
			name:        "re-serialized by a transformer, final frame unterminated",
			input:       "data: {\"a\":1}\n\ndata: [DONE]\n",
			transformer: reencodeTransformer{},
			wantEvents:  2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := framingReader(t, tt.input, 0, tt.transformer)
			assertWellFormedEvents(t, out, tt.wantEvents)
			if !strings.HasSuffix(out, "\n\n") {
				t.Fatalf("stream does not end with a blank line (final event would be discarded): %q", out)
			}
		})
	}
}

// TestSSETransformingReader_InjectedFrameStartsNewEvent pins that the frames
// the gateway injects at the end of a stream (synthetic finish_reason,
// gateway_error, [DONE]) are separated from the last forwarded event. Under the
// old framing they were appended to that event's data buffer, so the client
// received one event whose payload was the concatenation of two JSON objects.
func TestSSETransformingReader_InjectedFrameStartsNewEvent(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantEvents int
	}{
		{
			name:  "benign truncation: finish_reason seen, no [DONE], no separator",
			input: "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n",
			// content event + injected [DONE]
			wantEvents: 2,
		},
		{
			name:  "mid-generation cut: content, no finish_reason, no separator",
			input: "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n",
			// content event + gateway_error + [DONE]
			wantEvents: 3,
		},
		{
			name:       "empty stream: nothing forwarded, injected gateway_error",
			input:      "",
			wantEvents: 2,
		},
		{
			name:  "synthetic finish_reason before [DONE], no separator",
			input: "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\ndata: [DONE]\n",
			// The upstream put both lines in one event; the reader forwards the
			// content line, then synthesizes a finish_reason ahead of the
			// [DONE] it found on the next line: three separate events.
			wantEvents: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := framingReader(t, tt.input, 0, nil)
			assertWellFormedEvents(t, out, tt.wantEvents)
		})
	}
}

// TestSSETransformingReader_CanonicalPassthroughIsByteIdentical guards the
// framing fix against altering a well-formed upstream stream: nothing may be
// added, removed, or re-ordered when every frame is already blank-terminated.
// CRLF input is line-normalized to LF by the reader (a documented property of
// the line-oriented re-emission), so byte identity is pinned for LF streams and
// CRLF streams are checked for event equivalence instead.
func TestSSETransformingReader_CanonicalPassthroughIsByteIdentical(t *testing.T) {
	inputs := []string{
		"data: {\"a\":1}\n\ndata: {\"b\":2}\n\ndata: [DONE]\n\n",
		": keepalive\n\ndata: {\"a\":1}\n\ndata: [DONE]\n\n",
		"event: message\ndata: {\"a\":1}\n\ndata: [DONE]\n\n",
	}
	for _, input := range inputs {
		out := framingReader(t, input, 0, nil)
		if out != input {
			t.Errorf("well-formed stream was rewritten:\n in=%q\nout=%q", input, out)
		}
	}

	// CRLF: the events must be equivalent to the LF spelling of the same
	// stream, and every event must be terminated.
	crlf := "data: {\"a\":1}\r\n\r\ndata: [DONE]\r\n\r\n"
	lf := "data: {\"a\":1}\n\ndata: [DONE]\n\n"
	if got := framingReader(t, crlf, 0, nil); got != lf {
		t.Errorf("CRLF stream normalized to %q, want the LF spelling %q", got, lf)
	}
}

// TestSSETransformingReader_ChunkSplitInvariance pins the audit finding for the
// CRLF class: a CRLF (or LF) split across read-chunk boundaries must not change
// the output. The reader scans through bufio.Scanner, whose ScanLines drops a
// CR only once the LF is visible, so every split offset must agree.
func TestSSETransformingReader_ChunkSplitInvariance(t *testing.T) {
	inputs := map[string]string{
		"lf":                   "data: {\"a\":1}\n\ndata: {\"b\":2}\n\ndata: [DONE]\n\n",
		"crlf":                 "data: {\"a\":1}\r\n\r\ndata: {\"b\":2}\r\n\r\ndata: [DONE]\r\n\r\n",
		"crlf mixed with lf":   "data: {\"a\":1}\r\n\ndata: {\"b\":2}\n\r\ndata: [DONE]\n\n",
		"crlf, no final blank": "data: {\"a\":1}\r\n\r\ndata: [DONE]\r\n",
	}
	for name, input := range inputs {
		t.Run(name, func(t *testing.T) {
			base := framingReader(t, input, 0, nil)
			for size := 1; size <= len(input); size++ {
				got := framingReader(t, input, size, nil)
				if got != base {
					t.Fatalf("chunk size %d changed the output:\nbase=%q\n got=%q", size, base, got)
				}
			}
		})
	}
}

// TestSSETransformingReader_StrayCRHandling pins how lone carriage returns are
// treated, as exact fixtures: bufio.Scanner's ScanLines drops ONE CR before the
// LF (or at EOF), so a doubled CR leaves a CR inside the line — which must not
// break event framing, and which a JSON payload tolerates as trailing
// whitespace.
func TestSSETransformingReader_StrayCRHandling(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "cr at eof is a line terminator",
			in:   "data: {\"a\":1}\n\ndata: [DONE]\r",
			want: "data: {\"a\":1}\n\ndata: [DONE]\n\n",
		},
		{
			name: "doubled cr leaves one cr in the line",
			in:   "data: {\"a\":1}\r\r\n\r\ndata: [DONE]\n\n",
			want: "data: {\"a\":1}\r\n\ndata: [DONE]\n\n",
		},
		{
			name: "escaped cr inside the payload is untouched",
			in:   "data: {\"a\":\"x\\ry\"}\n\ndata: [DONE]\n\n",
			want: "data: {\"a\":\"x\\ry\"}\n\ndata: [DONE]\n\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := framingReader(t, tt.in, 0, nil)
			if out != tt.want {
				t.Fatalf("out = %q, want %q", out, tt.want)
			}
			assertWellFormedEvents(t, out, 2)
		})
	}
}

// TestSSETransformingReader_CancelledContextTerminatesFinalEvent pins that the
// cancellation path also closes the final event: the proxy writes its own
// terminal frame (e.g. the timeout terminator) onto the committed stream, and an
// unterminated last line would glue that frame onto the previous event.
func TestSSETransformingReader_CancelledContextTerminatesFinalEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewSSETransformingReader(strings.NewReader("data: {\"a\":1}\n"), nil, ctx)

	var out bytes.Buffer
	buf := make([]byte, 32)
	for i := 0; i < 8 && !strings.HasSuffix(out.String(), "data: {\"a\":1}\n"); i++ {
		n, err := r.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			t.Fatalf("unexpected error while draining the frame: %v", err)
		}
	}
	if got := out.String(); got != "data: {\"a\":1}\n" {
		t.Fatalf("forwarded bytes = %q, want the unterminated frame", got)
	}

	cancel()
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("the terminator must be delivered without an error, got %v", err)
	}
	if got := string(buf[:n]); got != "\n" {
		t.Fatalf("terminator = %q, want a single blank line", got)
	}
	out.Write(buf[:n])
	if !strings.HasSuffix(out.String(), "\n\n") {
		t.Fatalf("stream canceled mid-read left the final event unterminated: %q", out.String())
	}
	if _, err := r.Read(buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled after the terminator", err)
	}
}

// TestSSETransformingReader_CancelledContextDoesNotTruncateBufferedLine pins the
// ordering of Read's buffer serving: a partially delivered line must be
// completed before cancellation is honored, so the separator can never truncate
// it into a bogus event.
func TestSSETransformingReader_CancelledContextDoesNotTruncateBufferedLine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	line := "data: {\"a\":\"" + strings.Repeat("x", 200) + "\"}\n"
	r := NewSSETransformingReader(strings.NewReader(line+"\n"), nil, ctx)

	head := make([]byte, 16)
	n, err := r.Read(head)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	cancel()
	rest, err := io.ReadAll(r)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	got := string(head[:n]) + string(rest)
	if !strings.HasPrefix(got, "data: {\"a\":\"xxx") || !strings.Contains(got, "\"}\n") {
		t.Fatalf("the buffered line was truncated: %q", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("stream not terminated: %q", got)
	}
	assertWellFormedEvents(t, got, 1)
}

// FuzzSSETransformingReaderFraming pins two framing invariants for arbitrary
// upstream bytes: the produced stream always ends with a terminated event, and
// the output is independent of how the input is chunked (every split offset
// must agree, including splits inside a CRLF pair).
func FuzzSSETransformingReaderFraming(f *testing.F) {
	seeds := []string{
		"data: {\"a\":1}\n\ndata: [DONE]\n\n",
		"data: {\"a\":1}\r\n\r\ndata: [DONE]\r\n\r\n",
		"data: {\"a\":1}\n\ndata: [DONE]\n",
		"data: {\"a\":1}\r",
		"data: {",
		"\r\n\r\n",
		": comment\n\n",
		"data: [DONE]",
		"event: x\ndata: {\"a\":1}\n\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 2048 {
			t.Skip("input too large for the split-at-every-offset property")
		}
		base := framingReader(t, input, 0, nil)
		if _, unterminated := parseSSEEvents(t, base); unterminated {
			t.Fatalf("input %q produced an unterminated final event: %q", input, base)
		}
		for size := 1; size <= len(input); size++ {
			got := framingReader(t, input, size, nil)
			if got != base {
				t.Fatalf("chunk size %d changed the output for input %q:\nbase=%q\n got=%q", size, input, base, got)
			}
		}
	})
}

// errAfterReader yields its payload and then fails, so the reader sees a
// transport error in the middle of a line (not a clean EOF).
type errAfterReader struct {
	data []byte
	done bool
	err  error
}

func (e *errAfterReader) Read(p []byte) (int, error) {
	if !e.done {
		e.done = true
		n := copy(p, e.data)
		return n, nil
	}
	return 0, e.err
}

// TestSSETransformingReader_FilterBlockTerminatesPendingEvent pins the
// policy-block exit: when a filter flips to blocked mid-stream the reader must
// close the event it had already forwarded, because the proxy writes its own
// block frame (writeBlockedSSE / writeExfilBlockedSSE / writeExfilDetectedSSE)
// onto the same committed stream immediately afterwards — an unterminated last
// line would glue that frame onto the previous event's data.
func TestSSETransformingReader_FilterBlockTerminatesPendingEvent(t *testing.T) {
	canary := "NENYA-CANARY-0123456789abcdef0123456789abcdef"
	input := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"" + canary + "\"}}]}\n"

	r := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
	r.SetCanaryWatcher(NewCanaryWatcher(canary, config.CanaryActionBlock, "stream", nil, nil))

	var out bytes.Buffer
	_, err := io.Copy(&out, r)
	if !errors.Is(err, ErrCanaryDetected) {
		t.Fatalf("err = %v, want ErrCanaryDetected", err)
	}
	if got := out.String(); !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("pending event not closed before the block error: %q", got)
	}
	// Only the first (clean) frame is delivered; the canary frame is suppressed.
	assertWellFormedEvents(t, out.String(), 1)
}

// TestSSETransformingReader_TruncatedFinalFrameIsTerminated pins the scanner
// error path: a line the source never terminated is still closed. The reader
// must not withhold the terminator for a truncated (unparseable) tail — the
// proxy writes its own error frame onto the committed stream next, and a pending
// event would absorb it so the client never saw the failure. The trailing
// gateway_error/[DONE] (or the proxy frame) is what explains the cut.
func TestSSETransformingReader_TruncatedFinalFrameIsTerminated(t *testing.T) {
	transportErr := errors.New("transport reset")
	src := &errAfterReader{data: []byte("data: {\"partial"), err: transportErr}
	r := NewSSETransformingReader(src, nil, context.Background())

	var out bytes.Buffer
	_, err := io.Copy(&out, r)
	if !errors.Is(err, transportErr) {
		t.Fatalf("err = %v, want the transport error", err)
	}
	body := out.String()
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("truncated final frame left pending (a following gateway frame would be absorbed): %q", body)
	}
	events, unterminated := parseSSEEvents(t, body)
	if unterminated {
		t.Fatalf("stream ended pending: %q", body)
	}
	if len(events) != 1 || events[0].Data[0] != `{"partial` {
		t.Fatalf("got %v, want the single truncated frame as its own event: %q", events, body)
	}
}

// TestSSETransformingReader_PartialTailBeforeInjectedTerminal pins the
// benign-truncation shape: the complete frame is terminated, the unparseable
// tail is terminated too (the injected terminal must be its own event, not
// glued onto the tail), and the injected [DONE] is delivered as a clean event.
func TestSSETransformingReader_PartialTailBeforeInjectedTerminal(t *testing.T) {
	input := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"partial"
	out := framingReader(t, input, 0, nil)
	events, unterminated := parseSSEEvents(t, out)
	if unterminated {
		t.Fatalf("the injected terminal must close the stream: %q", out)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want content + truncated tail + [DONE]: %q", len(events), out)
	}
	if events[len(events)-1].Data[0] != "[DONE]" {
		t.Fatalf("final event = %q, want [DONE]: %q", events[len(events)-1].Data[0], out)
	}
}

// TestSSETransformingReader_TooLongLineTerminatesFinalEvent pins the scanner
// overflow path (bufio.ErrTooLong): the injected gateway_error is a complete,
// terminated event.
func TestSSETransformingReader_TooLongLineTerminatesFinalEvent(t *testing.T) {
	input := "data: " + strings.Repeat("x", SSEScannerMaxBuf+16) + "\n"
	r := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())

	var out bytes.Buffer
	if _, err := io.Copy(&out, r); err == nil {
		t.Fatal("expected a scanner error for an oversized line")
	}
	assertWellFormedEvents(t, out.String(), 2)
}

// TestSSETransformingReader_SuppressedCutStillTerminates pins the stream
// continuation path: with the cut error suppressed the reader ends with a plain
// EOF, but the last forwarded event is still terminated so the proxy's resumed
// stream (or its terminator) cannot glue onto it.
func TestSSETransformingReader_SuppressedCutStillTerminates(t *testing.T) {
	r := NewSSETransformingReader(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n"), nil, context.Background())
	r.SetSuppressCutError(true)

	var out bytes.Buffer
	if _, err := io.Copy(&out, r); err != nil && err != io.EOF {
		t.Fatalf("copy: %v", err)
	}
	if got := out.String(); !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("suppressed cut left the event unterminated: %q", got)
	}
	if strings.Contains(out.String(), "gateway_error") {
		t.Fatalf("suppressed cut must not inject a terminal: %q", out.String())
	}
	assertWellFormedEvents(t, out.String(), 1)
}

// TestSSETransformingReader_DiscardModeTerminatesFinalEvent pins the
// oversize-output path: after the reader starts discarding, the injected size
// error is still a complete terminated event.
func TestSSETransformingReader_DiscardModeTerminatesFinalEvent(t *testing.T) {
	input := "data: {\"choices\":[{\"delta\":{\"content\":\"aaaaaaaaaaaaaaaaaaaa\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"bbbbbbbbbbbbbbbbbbbb\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"cccccccccccccccccccc\"}}]}\n\n"
	r := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
	r.SetMaxTransformedBytes(64)

	var out bytes.Buffer
	_, err := io.Copy(&out, r)
	if !errors.Is(err, ErrTransformedSizeExceeded) {
		t.Fatalf("err = %v, want ErrTransformedSizeExceeded", err)
	}
	body := out.String()
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("discard mode left the stream unterminated: %q", body)
	}
	if !strings.Contains(body, "exceeded maximum size") {
		t.Fatalf("size error frame missing: %q", body)
	}
	events, unterminated := parseSSEEvents(t, body)
	if unterminated {
		t.Fatalf("stream ended pending: %q", body)
	}
	if events[len(events)-1].Data[0] != "[DONE]" {
		t.Fatalf("final event = %q, want [DONE]: %q", events[len(events)-1].Data[0], body)
	}
}

// TestSSETransformingReader_RetainedLineBufferNotRecycled pins the pooled
// scratch-buffer ownership: the reader retains the buffer backing the emission
// until the next line is fetched, so another stream recycling a pooled line
// buffer cannot overwrite bytes this stream has not delivered yet.
func TestSSETransformingReader_RetainedLineBufferNotRecycled(t *testing.T) {
	line := "data: {\"a\":\"" + strings.Repeat("x", 256) + "\"}\n"
	input := line + "\ndata: [DONE]\n\n"
	r := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())

	// Read only part of the line so the emission stays buffered.
	head := make([]byte, 16)
	n, err := r.Read(head)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}

	// Simulate other streams recycling pooled line buffers: with the reader
	// retaining its buffer, none of these can be the array backing the pending
	// emission. (Several are filled because the pool is process-wide and Get may
	// return any of its buffers.)
	recycled := make([]*[]byte, 0, 4)
	for i := 0; i < 4; i++ {
		other := GetLineCopyBuffer()
		full := (*other)[:cap(*other)]
		for j := range full {
			full[j] = 'Z'
		}
		recycled = append(recycled, other)
	}
	for _, other := range recycled {
		PutLineCopyBuffer(other)
	}

	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	got := string(head[:n]) + string(rest)
	if strings.Contains(got, "ZZZ") {
		t.Fatalf("retained line buffer was recycled and overwritten: %q", got)
	}
	if !strings.HasPrefix(got, "data: {\"a\":\"xxx") {
		t.Fatalf("emission corrupted: %q", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("stream not terminated: %q", got)
	}
}

// TestSSETransformingReader_CROnlyLineEndings_DocumentedLimitation pins a
// non-canonical framing shape that is NOT handled yet: bufio.Scanner's
// ScanLines splits on LF only, so a CR-only stream is read as one long line —
// [DONE] is not recognized and the reader injects a spurious gateway_error.
// WHATWG-legal CR-only framing is in scope for NENYA-197 (parser tolerance for
// non-canonical framing); this fixture makes any change to that behavior
// visible rather than silent.
func TestSSETransformingReader_CROnlyLineEndings_DocumentedLimitation(t *testing.T) {
	input := "data: {\"a\":1}\r\rdata: [DONE]\r\r"
	out := framingReader(t, input, 0, nil)

	// The exact current output: one glued line, a spurious injected
	// gateway_error, and an extra [DONE]. Any change (including a real fix in
	// NENYA-197) must update this fixture deliberately.
	want := "data: {\"a\":1}\r\rdata: [DONE]\r\n\n" +
		"data: {\"error\":{\"message\":\"upstream stream ended without [DONE]\",\"type\":\"gateway_error\"}}\n\n" +
		"data: [DONE]\n\n"
	if out != want {
		t.Fatalf("CR-only behavior changed — update this fixture and NENYA-197:\n got=%q\nwant=%q", out, want)
	}
}

// TestSSETransformingReader_MultiLineTransformerPayload_DocumentedContract pins
// the single-line contract of ResponseTransformer.TransformSSEChunk: only the
// first line of a multi-line return is prefixed with "data: ", so a transformer
// returning a raw newline produces one data line plus an unparsed field line.
func TestSSETransformingReader_MultiLineTransformerPayload_DocumentedContract(t *testing.T) {
	out := framingReader(t, "data: {\"a\":1}\n\ndata: [DONE]\n\n", 0, multiLineTransformer{})
	want := "data: {\"x\":1}\n{\"y\":2}\n\ndata: [DONE]\n\n"
	if out != want {
		t.Fatalf("out = %q, want %q (single-line transformer contract)", out, want)
	}
}

// multiLineTransformer violates the single-line TransformSSEChunk contract on
// purpose.
type multiLineTransformer struct{}

func (multiLineTransformer) TransformSSEChunk(_ context.Context, _ []byte) ([]byte, error) {
	return []byte("{\"x\":1}\n{\"y\":2}"), nil
}
