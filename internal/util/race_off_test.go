//go:build !race

package util

// raceEnabled reports whether this test binary was built with the race
// detector (false here: plain builds enforce the wall-clock ceiling).
const raceEnabled = false
