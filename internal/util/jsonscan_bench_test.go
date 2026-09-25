package util

import (
	"encoding/json"
	"strings"
	"testing"
)

var benchBody = []byte(`{"model":"qwen3-coder-plus","type":"standard","stream":true,"messages":[` +
	strings.Repeat(`{"role":"assistant","content":"`+strings.Repeat("x", 512)+`"},`, 20) +
	`{"role":"user","content":"fix the bug"}],"usage":{"prompt_tokens":100}}`)

func BenchmarkExtractTopLevelField(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := ExtractTopLevelField(benchBody, "type"); !ok {
			b.Fatal("not found")
		}
	}
}

// BenchmarkExtractFieldFullUnmarshal is the pre-NENYA-35 baseline for
// comparison in the perf-guard test.
func BenchmarkExtractFieldFullUnmarshal(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var data map[string]interface{}
		if err := json.Unmarshal(benchBody, &data); err != nil {
			b.Fatal(err)
		}
		if _, ok := data["type"]; !ok {
			b.Fatal("not found")
		}
	}
}

// TestPerfGuard_ExtractTopLevelField pins the regression ceilings
// (NENYA-35): the scanner must beat full unmarshal on allocations by a
// wide margin (a 20x512B-message body is production-shaped) and stay
// under a generous ns/op ceiling for slow CI machines.
func TestPerfGuard_ExtractTopLevelField(t *testing.T) {
	if testing.Short() {
		t.Skip("perf guard skipped in short mode")
	}

	scanner := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, ok := ExtractTopLevelField(benchBody, "type"); !ok {
				b.Fatal("not found")
			}
		}
	})
	full := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var data map[string]interface{}
			if err := json.Unmarshal(benchBody, &data); err != nil {
				b.Fatal(err)
			}
			if _, ok := data["type"]; !ok {
				b.Fatal("not found")
			}
		}
	})

	if scanner.AllocsPerOp() >= full.AllocsPerOp() {
		t.Errorf("scanner allocs/op (%d) must be strictly below full unmarshal (%d)",
			scanner.AllocsPerOp(), full.AllocsPerOp())
	}
	// Allocation ceiling: decoder + one RawMessage. Anything beyond 12
	// allocs/op means a regression reintroduced map materialization.
	if scanner.AllocsPerOp() > 12 {
		t.Errorf("alloc ceiling exceeded: %d allocs/op (ceiling 12)", scanner.AllocsPerOp())
	}
	// Generous ns/op ceiling (slow CI): 100µs. The scanner is ~2-4µs/op
	// locally; full unmarshal is ~40µs+ on this body. Under -race the
	// race detector inflates every memory access, so wall-clock numbers
	// are meaningless there: the timing guard is skipped (the alloc and
	// differential guards above remain in force).
	if raceEnabled {
		t.Logf("ns/op ceiling skipped under -race (measured %d ns/op)", scanner.NsPerOp())
	} else if scanner.NsPerOp() > 100_000 {
		t.Errorf("ns/op ceiling exceeded: %d ns/op (ceiling 100000)", scanner.NsPerOp())
	}
}
