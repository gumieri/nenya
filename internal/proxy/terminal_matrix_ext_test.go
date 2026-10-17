package proxy

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/nenya/internal/stream"
)

// TestBootstrapKeepalivesDoNotFlush pins the NENYA-174 keepalive half: SSE
// comments, empty data items, and pings keep the buffer open (undetermined)
// — none of them is mistaken for output or a rejection.
func TestBootstrapKeepalivesDoNotFlush(t *testing.T) {
	keepalives := []string{
		": keepalive",
		":",
		`data: {}`,
		`event: ping`,
		`data: [DONE]`,
		`{"type":"ping"}`,
	}
	for _, line := range keepalives {
		if v := classifyBootstrapLine([]byte(line)); v != bootstrapUndetermined {
			t.Fatalf("keepalive %q must keep the buffer open, got verdict %v", line, v)
		}
	}
}

// TestBootstrapHoldTimeCeiling pins the NENYA-174 time ceiling (the
// PRODUCTION predicate the bootstrap loop calls): a provider dripping
// handshake bytes forever is cut over to unbuffered streaming when the
// configured hold time elapses; a zero ceiling disables the time bound.
func TestBootstrapHoldTimeCeiling(t *testing.T) {
	if bootstrapHoldExceeded(1*time.Millisecond, 0) {
		t.Fatal("a zero ceiling must disable the time bound")
	}
	if !bootstrapHoldExceeded(5*time.Millisecond, 1*time.Millisecond) {
		t.Fatal("elapsed beyond the ceiling must trip the bound")
	}
	if bootstrapHoldExceeded(1*time.Millisecond, 1*time.Second) {
		t.Fatal("elapsed within the ceiling must not trip the bound")
	}
}

// TestPauseTurnNeverCachedAsComplete pins the NENYA-174 caching half: a
// captured stream whose terminal is Anthropic pause_turn must not be stored
// as a complete answer, while a normal end_turn capture is stored.
func TestPauseTurnNeverCachedAsComplete(t *testing.T) {
	paused := strings.Join([]string{
		`data: {"type":"message_start","message":{"model":"claude-sonnet-4"}}`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"pause_turn"}}`,
		"",
	}, "\n")
	if !capturedStreamHasResumableTerminal([]byte(paused)) {
		t.Fatal("the pause_turn terminal must classify as resumable")
	}
	complete := strings.Replace(paused, "pause_turn", "end_turn", 1)
	if capturedStreamHasResumableTerminal([]byte(complete)) {
		t.Fatal("a complete terminal must not classify as resumable")
	}
	// OpenAI-shaped captures: choice-level finish_reason (the real OpenAI
	// shape) and the delta.stop_reason surface.
	openaiPaused := `data: {"choices":[{"finish_reason":"pause_turn"}]}`
	if !capturedStreamHasResumableTerminal([]byte(openaiPaused)) {
		t.Fatal("the choice-level pause_turn terminal must classify as resumable")
	}
	deltaPaused := `data: {"choices":[{"delta":{"stop_reason":"pause_turn"}}]}`
	if !capturedStreamHasResumableTerminal([]byte(deltaPaused)) {
		t.Fatal("the delta-level pause_turn terminal must classify as resumable")
	}

	// The reason classifier itself.
	if !stream.IsResumableStopReasonName("pause_turn") || stream.IsResumableStopReasonName("end_turn") {
		t.Fatal("IsResumableStopReasonName classification drifted")
	}
}

// TestPauseTurnResumableFlagFromReader pins the review's high finding: on
// CONVERTED captures the transformer erases pause_turn (finish_reason says
// "stop"), so the cache gate consults the reader's raw-surface flag.
func TestPauseTurnResumableFlagFromReader(t *testing.T) {
	frames := "data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"pause_turn\"}}\n\n"
	reader := stream.NewSSETransformingReader(strings.NewReader(frames), nil, context.Background())
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !reader.SawResumableStop() {
		t.Fatalf("the reader must remember the raw pause_turn: %s", out)
	}

	// A complete turn does not set the flag.
	complete := strings.Replace(frames, "pause_turn", "end_turn", 1)
	reader2 := stream.NewSSETransformingReader(strings.NewReader(complete), nil, context.Background())
	if _, err := io.ReadAll(reader2); err != nil {
		t.Fatal(err)
	}
	if reader2.SawResumableStop() {
		t.Fatal("a complete turn must not set the resumable flag")
	}
}
