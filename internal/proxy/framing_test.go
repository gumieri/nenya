package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/stream"
)

// NENYA-169 — SSE framing on the gateway's own writers.
//
// The gateway writes frames of its own onto a committed stream (stall,
// timeout, policy block, canary/exfil block, mid-stream interruption). Two
// properties are pinned: every one of those frames is a complete, blank-line
// terminated SSE event carrying valid single-line JSON (a raw newline inside a
// data: payload corrupts the framing), and a frame written after the reader's
// output starts a NEW event rather than gluing onto the previous one.

// assertSSEEventsWellFormed asserts body parses as a sequence of terminated SSE
// events, each with exactly one data line whose payload is valid JSON or the
// [DONE] sentinel, and returns the payloads in order.
func assertSSEEventsWellFormed(t *testing.T, body string) []string {
	t.Helper()
	var payloads []string
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		if len(cur) != 1 {
			t.Fatalf("event carries %d data lines (frames were glued): %q", len(cur), body)
		}
		payloads = append(payloads, cur[0])
		cur = nil
	}
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			flush()
			continue
		}
		rest, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			t.Fatalf("non-data SSE line %q in %q", line, body)
		}
		cur = append(cur, rest)
	}
	if len(cur) > 0 {
		t.Fatalf("stream ended with an unterminated (pending) event: %q", body)
	}
	for _, p := range payloads {
		if p == "[DONE]" {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(p), &parsed); err != nil {
			t.Fatalf("payload is not valid single-line JSON (%v): %q", err, p)
		}
		if strings.Contains(p, "\n") {
			t.Fatalf("payload contains a raw newline, which corrupts data: framing: %q", p)
		}
	}
	return payloads
}

func TestStreamingErrorWriters_WellFormedFrames(t *testing.T) {
	p := &Proxy{}
	p.StoreGateway(newStreamTestGateway())
	gw := p.Gateway()

	writers := map[string]func(w http.ResponseWriter){
		"stall":          func(w http.ResponseWriter) { p.writeStallSSE(gw, w) },
		"timeout":        func(w http.ResponseWriter) { p.writeTimeoutSSE(gw, w) },
		"blocked":        func(w http.ResponseWriter) { p.writeBlockedSSE(gw, w) },
		"exfil detected": func(w http.ResponseWriter) { p.writeExfilDetectedSSE(gw, w) },
		"exfil blocked":  func(w http.ResponseWriter) { p.writeExfilBlockedSSE(gw, w) },
		"interrupted":    func(w http.ResponseWriter) { writeSSEError(nil, w, http.StatusOK, "upstream stream interrupted") },
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			write(rec)
			body := rec.Body.String()
			if !strings.HasSuffix(body, "\n\n") {
				t.Fatalf("frame is not blank-line terminated: %q", body)
			}
			payloads := assertSSEEventsWellFormed(t, body)
			if len(payloads) == 0 || payloads[len(payloads)-1] != "[DONE]" {
				t.Fatalf("stream must end with a [DONE] event, got %q", body)
			}
		})
	}
}

// TestWriteStructuredError_CompactSingleLine pins the error-body shape on the
// non-streaming path: the client parses it as JSON, and a body written onto a
// stream must not contain raw newlines that would corrupt data: framing. One
// trailing newline is the json.Encoder terminator.
func TestWriteStructuredError_CompactSingleLine(t *testing.T) {
	rec := httptest.NewRecorder()
	writeStructuredError(rec, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "model \"x\" is not chat-routable")

	body := rec.Body.String()
	if got := strings.Count(body, "\n"); got != 1 || !strings.HasSuffix(body, "\n") {
		t.Fatalf("body must be one line plus a terminator, got %d newlines: %q", got, body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var parsed infra.ErrorResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("body is not valid JSON: %v (%q)", err, body)
	}
	if parsed.Kind != infra.ErrorKindInvalidRequest {
		t.Fatalf("error_kind = %q, want %q", parsed.Kind, infra.ErrorKindInvalidRequest)
	}
}

// TestStreamingErrorFrameAfterUnterminatedReaderOutput pins the composition of
// the two fixes: the reader closes its final event even when the upstream
// omitted the trailing blank line, so a terminal frame the proxy writes onto
// the same committed stream starts a new event instead of being appended to the
// previous event's data buffer.
func TestStreamingErrorFrameAfterUnterminatedReaderOutput(t *testing.T) {
	// The upstream ends cleanly ([DONE]) but omits the trailing blank line, so
	// the reader forwards two frames and has to add the terminator itself.
	upstream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n"
	r := stream.NewSSETransformingReader(strings.NewReader(upstream), nil, context.Background())
	var committed bytes.Buffer
	if _, err := io.Copy(&committed, r); err != nil && err != io.EOF {
		t.Fatalf("copy: %v", err)
	}
	if got := committed.String(); !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("reader left the final event unterminated: %q", got)
	}

	rec := httptest.NewRecorder()
	_, _ = rec.Write(committed.Bytes())
	writeSSEError(nil, rec, http.StatusOK, "upstream stream interrupted")

	payloads := assertSSEEventsWellFormed(t, rec.Body.String())
	// content, synthesized finish_reason, [DONE], terminal gateway_error,
	// terminal [DONE].
	if len(payloads) != 5 {
		t.Fatalf("got %d events, want the three forwarded/synthesized frames plus the two terminal frames: %q", len(payloads), rec.Body.String())
	}
	if !strings.Contains(payloads[3], "upstream stream interrupted") {
		t.Fatalf("terminal frame is not its own event: %q", rec.Body.String())
	}
}

// TestPolicyBlockFrameAfterReaderBlockExit pins the composition of the reader's
// block exit and the gateway's policy-block writers: when a filter (canary
// tripwire, egress guard, execution policy) terminates the stream mid-flight the
// reader closes the event it had already forwarded, so the block frame the proxy
// writes next (writeExfilDetectedSSE here) is a separate event rather than being
// appended to the previous event's data buffer.
func TestPolicyBlockFrameAfterReaderBlockExit(t *testing.T) {
	canary := "NENYA-CANARY-0123456789abcdef0123456789abcdef"
	upstream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"" + canary + "\"}}]}\n"

	r := stream.NewSSETransformingReader(strings.NewReader(upstream), nil, context.Background())
	r.SetCanaryWatcher(stream.NewCanaryWatcher(canary, config.CanaryActionBlock, "stream", nil, nil))

	var committed bytes.Buffer
	if _, err := io.Copy(&committed, r); !errors.Is(err, stream.ErrCanaryDetected) {
		t.Fatalf("reader err = %v, want ErrCanaryDetected", err)
	}

	rec := httptest.NewRecorder()
	_, _ = rec.Write(committed.Bytes())
	p := &Proxy{}
	p.StoreGateway(newStreamTestGateway())
	p.writeExfilDetectedSSE(p.Gateway(), rec)

	payloads := assertSSEEventsWellFormed(t, rec.Body.String())
	// forwarded content, block frame, [DONE].
	if len(payloads) != 3 {
		t.Fatalf("got %d events, want content + block + [DONE]: %q", len(payloads), rec.Body.String())
	}
	if !strings.Contains(payloads[1], string(infra.ErrorKindExfilDetected)) {
		t.Fatalf("block frame is not its own event: %q", rec.Body.String())
	}
}
