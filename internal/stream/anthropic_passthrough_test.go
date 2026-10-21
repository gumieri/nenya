package stream

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// A complete native-Anthropic passthrough stream (nil transformer — the
// NENYA-176 anthropic-client/anthropic-upstream path) must end exactly where
// the upstream ended: no injected gateway_error, no appended data: [DONE], no
// breaker charge downstream (injectedErr stays false). Before the AUD-01 fix
// the terminal-state matrix read only OpenAI shapes, classified this stream
// EMPTY at EOF, and injected a gateway_error after the client's own
// message_stop.
func TestAnthropicPassthroughCompleteStreamEndsCleanly(t *testing.T) {
	const wire = "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\"}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"event: content_block_stop\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	reader := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	var emptyFired atomic.Int64
	reader.SetEmptyStreamCallback(func() { emptyFired.Add(1) })

	out := drainReader(t, reader)

	if reader.InjectedError() {
		t.Fatalf("a complete Anthropic passthrough must not inject a gateway_error; output tail: %q", tail(out, 160))
	}
	if !reader.SawContent() {
		t.Fatal("content_block_delta text must register as content for the terminal-state matrix")
	}
	if !reader.SawFinishReason() {
		t.Fatal("message_delta stop_reason / message_stop must register as a completion signal")
	}
	if out != wire {
		t.Fatalf("a complete passthrough must be byte-faithful (no injected frames)\nwant: %q\ngot:  %q", wire, out)
	}
	if emptyFired.Load() != 0 {
		t.Fatal("the empty-200 degradation signal must not fire for a completed passthrough")
	}
}

// A passthrough cut (deltas seen, no message_delta/message_stop) keeps the
// genuine-cut classification: content seen, no completion, gateway_error
// injected — never the empty-stream branch.
func TestAnthropicPassthroughCutStillInjected(t *testing.T) {
	wire := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"

	reader := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	out := drainReader(t, reader)

	if !reader.InjectedError() {
		t.Fatal("a mid-generation passthrough cut must still inject the gateway_error terminal")
	}
	if !reader.SawContent() || reader.SawFinishReason() {
		t.Fatalf("cut classification wrong: sawContent=%v sawFinishReason=%v", reader.SawContent(), reader.SawFinishReason())
	}
	if !strings.Contains(out, "gateway_error") {
		t.Fatal("the cut terminal must carry the gateway_error event")
	}
}

// Tool-argument deltas (input_json_delta partial_json) are generated content:
// a tool-use passthrough that terminates cleanly must not be classified empty.
func TestAnthropicPassthroughToolArgumentsCountAsContent(t *testing.T) {
	wire := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get_weather\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"Oslo\\\"}\"}}\n\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	reader := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	out := drainReader(t, reader)

	if reader.InjectedError() {
		t.Fatalf("a completed tool-use passthrough must end cleanly; tail: %q", tail(out, 160))
	}
	if !reader.SawContent() {
		t.Fatal("input_json_delta partial_json must register as content")
	}
}

// A complete but empty Anthropic message (message_start + message_stop, no
// blocks) is a valid completion, not an empty 200: it ends cleanly.
func TestAnthropicPassthroughEmptyMessageEndsCleanly(t *testing.T) {
	wire := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	reader := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	var emptyFired atomic.Int64
	reader.SetEmptyStreamCallback(func() { emptyFired.Add(1) })

	out := drainReader(t, reader)

	if reader.InjectedError() {
		t.Fatalf("a valid empty Anthropic message must not be classified as an empty stream; tail: %q", tail(out, 160))
	}
	if emptyFired.Load() != 0 {
		t.Fatal("the empty-200 signal must not fire for a completed (if empty) message")
	}
}

// A stream that carries no Anthropic events at all keeps the empty-stream
// classification and its NENYA-160 signal.
func TestAnthropicSurfaceFlagDoesNotMaskTrueEmptyStreams(t *testing.T) {
	reader := NewSSETransformingReader(strings.NewReader(""), nil, context.Background())
	var emptyFired atomic.Int64
	reader.SetEmptyStreamCallback(func() { emptyFired.Add(1) })

	drainReader(t, reader)

	if !reader.InjectedError() {
		t.Fatal("a truly empty stream must still inject the gateway_error terminal")
	}
	if emptyFired.Load() != 1 {
		t.Fatalf("a truly empty stream must fire the empty-200 signal exactly once, got %d", emptyFired.Load())
	}
}

// The recognition is gated on the passthrough condition: OpenAI-shaped streams
// (no transformer, no Anthropic envelope) keep the injectDoneOnly ending for a
// finish_reason without [DONE].
func TestOpenAIShapedFinishWithoutDoneStillInjectsDone(t *testing.T) {
	wire := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"

	reader := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	out := drainReader(t, reader)

	if reader.InjectedError() {
		t.Fatal("a finish_reason without [DONE] is a benign truncation: no gateway_error")
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatal("the OpenAI-shaped benign truncation must still receive the silent [DONE]")
	}
}

// Reset clears the completion flag: a reused reader must re-classify from its
// new stream, not inherit the previous stream's Anthropic terminal state.
func TestAnthropicSurfaceFlagResets(t *testing.T) {
	reader := NewSSETransformingReader(
		strings.NewReader("data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"),
		nil, context.Background())
	drainReader(t, reader)
	if !reader.sawAnthropicFinish {
		t.Fatal("precondition: the passthrough must mark the Anthropic completion surface")
	}

	reader.ResetCounters()
	if reader.sawAnthropicFinish || reader.sawContent || reader.sawFinishReason {
		t.Fatal("ResetCounters must clear the Anthropic completion flag and the terminal-state signals")
	}
}

// A complete passthrough whose final fragment is withheld by the ExfilGuard
// (NENYA-205 hold) must still receive the drained tail at the clean end —
// shaped as an Anthropic content_block_delta, never an OpenAI frame, and with
// no injected [DONE]/gateway_error. Before the held-flush drain was added to
// the Anthropic clean-end path, the withheld tail was silently lost AND the
// tail-less capture was cached.
func TestAnthropicPassthroughHeldTailFlushedAtCleanEnd(t *testing.T) {
	wire := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"see http://127.0.0.1:8080/steal\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	reader := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	reader.SetExfilGuard(stripGuard())

	out := drainReader(t, reader)

	if reader.InjectedError() {
		t.Fatalf("a completed passthrough must not inject a gateway_error; tail: %q", tail(out, 160))
	}
	if strings.Contains(out, "127.0.0.1:8080") {
		t.Fatalf("the violating URL leaked on the wire:\n%s", out)
	}
	if !strings.Contains(out, "EXFIL LINK REMOVED") {
		t.Fatalf("the withheld tail was lost at the clean end — the placeholder flush is missing:\n%s", out)
	}
	if !strings.Contains(out, `"content_block_delta"`) {
		t.Fatalf("the flush frame must follow the Anthropic surface:\n%s", out)
	}
	if strings.Contains(out, `"choices"`) {
		t.Fatalf("an Anthropic passthrough must not receive an OpenAI-shaped frame:\n%s", out)
	}
	if strings.Contains(out, "data: [DONE]") {
		t.Fatalf("no OpenAI [DONE] may be appended after the client's message_stop:\n%s", out)
	}
	// The tail must PRECEDE the terminal event: a strict Anthropic client
	// discards post-message_stop events, so the drained placeholder is
	// composed ahead of the terminal line (pre-terminal drain).
	ph := strings.Index(out, "EXFIL LINK REMOVED")
	stop := strings.Index(out, `"type":"message_stop"`)
	if ph == -1 || stop == -1 || ph > stop {
		t.Fatalf("the drained tail must precede the client's message_stop:\n%s", out)
	}
}

// AUD-02 (NENYA-210): the AnthropicTransformer synthesizes its own [DONE] at
// message_stop, and that synthesized terminal used to bypass
// finishStreamFrames — so the exfil guard's withheld tail (NENYA-205 hold) was
// silently dropped at stream end on the Anthropic-upstream → OpenAI-client
// path. The synthesized [DONE] must compose the held flush exactly like the
// raw [DONE] path does.
func TestTransformerSynthesizedDoneFlushesHeldTail(t *testing.T) {
	wire := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\"}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"see http://127.0.0.1:8080/steal\"}}\n\n" +
		"event: content_block_stop\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	reader := NewSSETransformingReader(strings.NewReader(wire), NewAnthropicTransformer(), context.Background())
	reader.SetExfilGuard(stripGuard())

	out := drainReader(t, reader)

	if strings.Contains(out, "127.0.0.1:8080") {
		t.Fatalf("the violating URL leaked on the wire:\n%s", out)
	}
	if !strings.Contains(out, "EXFIL LINK REMOVED") {
		t.Fatalf("the withheld tail was lost at the transformer's synthesized [DONE]:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("the OpenAI client must still receive the finish_reason frame:\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("the terminal [DONE] is missing:\n%s", out)
	}
	// The tail must precede the terminal [DONE], not land after it.
	ph := strings.Index(out, "EXFIL LINK REMOVED")
	done := strings.Index(out, "data: [DONE]")
	if ph == -1 || done == -1 || ph > done {
		t.Fatalf("the drained tail must precede the terminal [DONE]:\n%s", out)
	}
}

// A spoofed bare message_stop (no preceding message_delta — no real
// terminal-sequence evidence) must NOT trigger the pre-terminal drain, and
// the guard must stay armed for later chunks: post-stop violating content is
// still withheld/stripped, never forwarded. Before the completion-evidence
// gate + Rearm, one upstream-controlled message_stop permanently disarmed
// the egress control. The wire's message_stop still counts as the terminal
// for the end classification (the client saw it); the security property is
// that nothing withheld ever leaks.
func TestAnthropicSpoofedStopDoesNotDisarmExfilGuard(t *testing.T) {
	wire := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"see http://127.0.0.1:8080/steal \"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"and http://10.0.0.5/exfil\"}}\n\n"

	reader := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	reader.SetExfilGuard(stripGuard())

	out := drainReader(t, reader)

	for _, leak := range []string{"127.0.0.1:8080", "10.0.0.5"} {
		if strings.Contains(out, leak) {
			t.Fatalf("violating URL %q reached the client after a spoofed message_stop:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "EXFIL LINK REMOVED") {
		t.Fatalf("the withheld content must reach the client as placeholders at the terminal:\n%s", out)
	}
}

// A message_stop-only completion (no message_delta) that carries a held exfil
// tail composes flush + terminal in ONE emission; the terminal's completion
// signal must still register (per-line progress tracking) — the clean-end
// classification must not regress to the cut branch (gateway_error after the
// client's message_stop).
func TestMessageStopOnlyCompletionWithHeldTailEndsCleanly(t *testing.T) {
	wire := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"see http://127.0.0.1:8080/steal\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	reader := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	reader.SetExfilGuard(stripGuard())

	out := drainReader(t, reader)

	if reader.InjectedError() {
		t.Fatalf("the message_stop completion must register through the composed emission; tail: %q", tail(out, 160))
	}
	if !strings.Contains(out, "EXFIL LINK REMOVED") {
		t.Fatalf("the withheld tail was lost:\n%s", out)
	}
	if strings.Contains(out, "gateway_error") {
		t.Fatalf("no gateway_error may follow the client's message_stop:\n%s", out)
	}
}

// The bare-JSON variant of the synthesized terminal (sseDataLine convention:
// raw JSON bodies under text/event-stream) gets the same composition as the
// data-line variant — a canonical, prefixed [DONE] frame, not a bare value
// strict clients would ignore. (Exfil scanning of bare-JSON lines is a
// separate pre-existing gap, tracked with the AUD-04 egress-shape family.)
func TestBareJSONMessageStopComposesTerminal(t *testing.T) {
	wire := "{\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\"}}\n\n" +
		"{\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"{\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"{\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"{\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"{\"type\":\"message_stop\"}\n\n"

	reader := NewSSETransformingReader(strings.NewReader(wire), NewAnthropicTransformer(), context.Background())

	out := drainReader(t, reader)

	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("the bare-JSON synthesized terminal must be re-framed as a canonical data: [DONE]:\n%s", out)
	}
	if !strings.Contains(out, `"content":"hello"`) {
		t.Fatalf("the converted content chunk is missing:\n%s", out)
	}
	if !reader.SawDone() {
		t.Fatal("the bare-JSON terminal must register sawDone for the terminal-state matrix")
	}
}

// Content arriving AFTER a real (fully-evidenced) terminal sequence —
// message_delta + message_stop, i.e. the case where the pre-terminal drain
// legitimately fires — must still be filtered: the drain re-arms the guard,
// so a post-terminal violating chunk is withheld and delivered as a
// placeholder at the clean end. Without the Rearm, the drained guard would
// pass the violation through verbatim.
func TestPostTerminalContentStaysFilteredAfterDrain(t *testing.T) {
	wire := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"see http://127.0.0.1:8080/steal \"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"and http://10.0.0.5/exfil\"}}\n\n"

	reader := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	reader.SetExfilGuard(stripGuard())

	out := drainReader(t, reader)

	for _, leak := range []string{"127.0.0.1:8080", "10.0.0.5"} {
		if strings.Contains(out, leak) {
			t.Fatalf("violating URL %q reached the client after the guard drained:\n%s", leak, out)
		}
	}
	if strings.Count(out, "EXFIL LINK REMOVED") != 2 {
		t.Fatalf("both withheld fragments must reach the client as placeholders:\n%s", out)
	}
	if reader.InjectedError() {
		t.Fatalf("the evidenced terminal sequence is a completion; tail: %q", tail(out, 160))
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
