//go:build race

package util

// raceEnabled reports whether this test binary was built with the race
// detector. Wall-clock assertions are skipped under -race because
// instrumentation inflates timings by an order of magnitude; the
// deterministic allocation and differential guards still apply.
const raceEnabled = true
