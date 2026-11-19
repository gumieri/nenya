package proxy

// MCP-loop accumulator benchmarks over the fixture corpus (5a8943f0): the
// buffered loop parses every upstream line through the accumulator per
// iteration — this is the per-request cost of the NENYA-337 and OpenAI
// shapes on the loop's hot path.

import (
	"strings"
	"testing"

	mcpproxy "github.com/nenya/internal/proxy/mcp"
	"github.com/nenya/internal/ssefixtures"
)

func benchmarkAccumulator(b *testing.B, format, wire string) {
	// Every split line — production's scanner feeds blank lines to
	// ProcessLine too (it early-returns on them).
	lines := strings.Split(wire, "\n")
	// Pre-timer consumption check keyed to the FORMAT's terminal contract:
	// OpenAI chat ends with [DONE] (ProcessLine returns true there);
	// Responses never sends [DONE] — consumption is verified by data-line
	// count and the model capture instead. A parser regression returning
	// false everywhere (or dropping lines) would otherwise benchmark
	// ~nothing silently.
	dataLines := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "data: ") && strings.TrimSpace(strings.TrimPrefix(line, "data: ")) != "[DONE]" {
			dataLines++
		}
	}
	probe := mcpproxy.NewSSEAccumulator(nil)
	probe.Format = format
	sawDone := false
	for _, line := range lines {
		if probe.ProcessLine(line) {
			sawDone = true
		}
	}
	switch format {
	case "openai":
		// [DONE] proves the terminal was seen; Model != "" and the
		// data-line count prove the chunks actually parsed (ProcessLine
		// returns true on the bare [DONE] literal regardless of parse
		// health). probe.DataLines counts the [DONE] line too, hence +1.
		if !sawDone || probe.Model == "" || probe.DataLines != dataLines+1 {
			b.Fatalf("accumulator state under-consumed: done=%v model=%q dataLines=%d/%d — the benchmark would measure nothing",
				sawDone, probe.Model, probe.DataLines, dataLines+1)
		}
	default:
		if probe.DataLines != dataLines || probe.Model == "" {
			b.Fatalf("accumulator consumed %d/%d data lines, model %q — the benchmark would measure nothing",
				probe.DataLines, dataLines, probe.Model)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		acc := mcpproxy.NewSSEAccumulator(nil)
		acc.Format = format
		for _, line := range lines {
			acc.ProcessLine(line)
		}
	}
}

func BenchmarkAccumulator_ProcessLine_OpenAI(b *testing.B) {
	benchmarkAccumulator(b, "openai", ssefixtures.OpenAIToolCallStream())
}

func BenchmarkAccumulator_ProcessLine_Responses(b *testing.B) {
	benchmarkAccumulator(b, "openai-responses", ssefixtures.ResponsesToolStream())
}
