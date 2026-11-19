package stream

// Hot-path benchmarks over the SSE fixture corpus (5a8943f0): the
// transformer + transforming-reader paths convert the shapes real
// upstreams send. Baseline for future hot-path work — run with
//
//	go test ./internal/stream/ -bench 'BenchmarkResponses|BenchmarkAnthropic' -benchmem -run '^$'
//
// and compare with benchstat.

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/nenya/internal/ssefixtures"
)

// benchDataLines converts a fixture into the [][]byte lines the scanner
// hands the pipeline, once outside the timer.
func benchDataLines(wire string) [][]byte {
	var lines [][]byte
	for _, line := range strings.Split(wire, "\n") {
		if strings.HasPrefix(line, "data: ") {
			lines = append(lines, []byte(strings.TrimPrefix(line, "data: ")))
		}
	}
	return lines
}

// anthropicFixtureEmittedFrames is the exact number of client-visible
// frames AnthropicThinkingToolStream converts to (thinking delta, text
// delta, tool-use opening, 2 argument deltas, finish chunk, [DONE] —
// deterministic wire → deterministic count).
const anthropicFixtureEmittedFrames = 7

// feedTransformer drives one wire through any transformer, counting emitted
// frames — a regression that consumes or errors on every event would
// otherwise benchmark as faster with no signal.
func feedTransformer(b *testing.B, tr ResponseTransformer, lines [][]byte) int {
	ctx := context.Background()
	emitted := 0
	for _, line := range lines {
		_, err := tr.TransformSSEChunk(ctx, line)
		if err != nil && !errors.Is(err, ErrEventConsumed) {
			b.Fatalf("transform: %v", err)
		}
		if err == nil {
			emitted++
		}
	}
	return emitted
}

// responsesFixtureEmittedFrames is the exact number of client-visible
// chunks the ResponsesToolStream fixture converts to (deterministic wire →
// deterministic count; the benches assert it so a regression dropping half
// the frames cannot benchmark silently).
const responsesFixtureEmittedFrames = 7

// BenchmarkResponsesTransformer_FullToolStream measures one complete
// tool-call stream through the Responses transformer with a fresh instance
// per iteration — the real per-request cost (the transformer is stateful and
// never reused within a stream).
func BenchmarkResponsesTransformer_FullToolStream(b *testing.B) {
	lines := benchDataLines(ssefixtures.ResponsesToolStream())
	// Pre-timer validation pins the exact emitted count; the hot loop
	// asserts it per iteration (one predictable int compare).
	if emitted := feedTransformer(b, NewResponsesTransformer(), lines); emitted != responsesFixtureEmittedFrames {
		b.Fatalf("fixture emitted %d frames, want %d — the fixture or transformer drifted", emitted, responsesFixtureEmittedFrames)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := feedTransformer(b, NewResponsesTransformer(), lines); got != responsesFixtureEmittedFrames {
			b.Fatalf("iteration %d emitted %d frames, want %d", i, got, responsesFixtureEmittedFrames)
		}
	}
}

// BenchmarkSSETransformingReader_ResponsesWire measures the full reader path
// (scanner + transform + framing + usage callback) over the Responses wire —
// what the client-facing goroutine actually does.
func BenchmarkSSETransformingReader_ResponsesWire(b *testing.B) {
	wire := ssefixtures.ResponsesToolStream()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reader := NewSSETransformingReader(strings.NewReader(wire), NewResponsesTransformer(), ctx)
		reader.SetOnUsage(func(UsageData) {})
		if _, err := io.Copy(io.Discard, reader); err != nil {
			b.Fatalf("read: %v", err)
		}
	}
}

// BenchmarkAnthropicTransformer_FullThinkingToolStream measures one complete
// thinking+tool-use stream through the Anthropic transformer (the
// anthropic-format target path and the NENYA-51 composite's client stage).
func BenchmarkAnthropicTransformer_FullThinkingToolStream(b *testing.B) {
	lines := benchDataLines(ssefixtures.AnthropicThinkingToolStream())
	// Pre-timer guard pins the exact emitted count (same discipline as the
	// Responses bench); the hot loop asserts it per iteration.
	if emitted := feedTransformer(b, NewAnthropicTransformer(), lines); emitted != anthropicFixtureEmittedFrames {
		b.Fatalf("fixture emitted %d frames, want %d — the fixture or transformer drifted", emitted, anthropicFixtureEmittedFrames)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := feedTransformer(b, NewAnthropicTransformer(), lines); got != anthropicFixtureEmittedFrames {
			b.Fatalf("iteration %d emitted %d frames, want %d", i, got, anthropicFixtureEmittedFrames)
		}
	}
}
