package stream

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nenya/config"
)

// exfilStripGrowthBound bounds per-construct amplification: strip mode
// replaces a violating construct (shortest form 5 bytes, `[](x)`) with a
// 27-byte placeholder — ≤5.4x, ≈6.2x with the streaming window's
// prefix split. The 8x allowance plus this constant leaves headroom
// while still catching runaway rewriting.
const exfilStripGrowthBound = 64

// FuzzExfilURLScan exercises the output-side ExfilGuard over arbitrary
// content on both evaluation paths: the streaming sliding window
// (FilterContent, fed in sequential chunks so boundary straddling is
// exercised) and the buffered full-body scan (InspectFull). The guard
// must terminate without panicking, must not grow content beyond the
// per-construct placeholder bound, and must preserve UTF-8 validity for
// valid input (JSON decoding guarantees valid UTF-8 in production;
// invalid input is covered as robustness).
//
// Seeds include split-URL exfil attempts (a violating URL straddling
// chunk boundaries), query-string payload smuggling, markdown image
// beacons, and benign prose.
func FuzzExfilURLScan(f *testing.F) {
	seeds := []string{
		// Benign prose with no URLs.
		"hello world, no links here",
		"",
		strings.Repeat("plain text. ", 100),
		// Public links (allowed by default).
		"see [docs](https://example.com/docs?page=1) for details",
		"https://example.com/path",
		// Exfil shapes: private IP literal hosts.
		"![x](http://127.0.0.1:8080/steal?data=abc)",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/",
		// Query smuggling / high entropy.
		"https://example.com/?q=" + strings.Repeat("QWxhZGRpbjpvcGVuIHNlc2FtZQ", 12),
		// Split-URL exfil attempts (head/tail split across chunks).
		"![beacon](http://10.0.0.1/exfil?d=",
		"aGVsbG8gd29ybGQgdGhpcyBpcyBhIGxvbmcgYmFzZTY0IHBheWxvYWQ",
		// Delimiter and angle-bracket shapes.
		"<https://example.com/x>",
		"https://example.com/path)trailing",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, content string) {
		if len(content) > 20000 {
			return
		}
		cfg := config.ExfilGuardConfig{
			Enabled: config.PtrTo(true),
			Action:  config.ExfilActionStrip,
		}

		// Streaming path: sliding window, fed in two chunks so a
		// construct split across the boundary is still evaluated. The
		// split is rune-aligned: real SSE deltas are JSON-decoded and
		// therefore always valid UTF-8, so a mid-rune split would test
		// an unreachable input shape (see checkUTF8Preserved).
		guard := NewExfilGuard(cfg, nil)
		mid := runeSafeSplit(content)
		streamed := ""
		for _, chunk := range []string{content[:mid], content[mid:]} {
			out, _, _ := guard.FilterContent(chunk)
			streamed += out
		}
		if len(streamed) > len(content)*8+exfilStripGrowthBound {
			t.Errorf("stream filter grew content unexpectedly: %d -> %d", len(content), len(streamed))
		}
		checkUTF8Preserved(t, "stream", content, streamed)

		// Buffered path: full-body inspection.
		buffered := NewExfilGuard(cfg, nil)
		full, _, _ := buffered.InspectFull(content)
		if len(full) > len(content)*8+exfilStripGrowthBound {
			t.Errorf("buffered filter grew content unexpectedly: %d -> %d", len(content), len(full))
		}
		checkUTF8Preserved(t, "buffered", content, full)

	})
}

// TestExfilFuzzBlockSeed pins the block-action contract the fuzz target
// seeds exercise: a violating construct must both report ActionBlock and
// latch the guard into the blocked state.
func TestExfilFuzzBlockSeed(t *testing.T) {
	cfg := config.ExfilGuardConfig{Enabled: config.PtrTo(true), Action: config.ExfilActionBlock}
	guard := NewExfilGuard(cfg, nil)
	if _, action, reason := guard.InspectFull("[](http://127.0.0.1/x)"); action != ActionBlock {
		t.Fatalf("action = %v, want ActionBlock (reason %q)", action, reason)
	}
	if !guard.IsBlocked() {
		t.Error("block action must latch the guard into the blocked state")
	}
}

// runeSafeSplit returns a byte index near the middle of s that does not
// split a UTF-8 rune (0 or len(s) for short/invalid input).
func runeSafeSplit(s string) int {
	mid := len(s) / 2
	for mid > 0 && mid < len(s) && !utf8.RuneStart(s[mid]) {
		mid--
	}
	return mid
}

// checkUTF8Preserved asserts the guard never turns valid input into
// invalid output. Invalid input (unreachable in production: JSON
// decoding guarantees valid UTF-8) only needs to terminate without
// panicking — the rune window may legitimately pass stray bytes
// through while still stripping constructs it recognizes.
func checkUTF8Preserved(t *testing.T, path, in, out string) {
	t.Helper()
	if utf8.ValidString(in) && !utf8.ValidString(out) {
		t.Errorf("%s filter produced invalid UTF-8 from valid input", path)
	}
}
