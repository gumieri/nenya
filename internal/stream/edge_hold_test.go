package stream

import (
	"context"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nenya/config"
)

// NENYA-205 fixtures: the ExfilGuard withholds the whitespace-free tail
// of the stream (it could still grow into a URL construct), so a
// violating URL split across any chunking — at every possible offset,
// including inside multi-byte runes — is spliced whole instead of
// streaming its head. The held tail is emitted at stream end (Flush /
// the reader's terminal flush), so benign responses neither lose nor
// duplicate their last fragment. These tests assert CLIENT-VISIBLE
// CONCATENATIONS: raw wire Contains checks are how the original vacuous
// straddle test survived.

// exfilAuditContent is the audit reproduction: a violating private URL
// inside a markdown image, surrounded by prose and multi-byte runes.
const exfilAuditContent = "bet密文 texts ![bing](http://127.0.0.1:8080/x?d=AAAA) 説明 🎉 tail"

// feedGuard streams content through guard in chunks of chunkSize runes
// (varying it exercises every straddle offset) and returns the
// client-visible concatenation (held tail flushed at stream end) plus
// whether the guard blocked. Suppressed (blocked) frames contribute
// nothing — callers never forward them.
func feedGuard(guard *ExfilGuard, content string, chunkSize int) (string, bool) {
	var out strings.Builder
	blocked := false
	runes := []rune(content)
	for start := 0; start < len(runes); {
		end := start + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		got, action, _ := guard.FilterContent(string(runes[start:end]))
		if action == ActionBlock {
			blocked = true
			start = end
			continue
		}
		out.WriteString(got)
		start = end
	}
	held, _ := guard.Flush()
	out.WriteString(held)
	return out.String(), blocked
}

// TestExfilGuardEveryOffsetHoldsPartialMatches is the exhaustive audit
// fixture: every chunk size 1..N runes (the construct straddles at every
// possible offset, including inside multi-byte runes) across all three
// actions. Strip must never let the host reach the client; block must
// always fire and must never forward the host; log must be byte-identical.
func TestExfilGuardEveryOffsetHoldsPartialMatches(t *testing.T) {
	total := len([]rune(exfilAuditContent))
	for chunkSize := 1; chunkSize <= total; chunkSize++ {
		t.Run("chunk"+strconv.Itoa(chunkSize), func(t *testing.T) {
			got, blocked := feedGuard(stripGuard(), exfilAuditContent, chunkSize)
			if blocked {
				t.Fatal("strip guard must not block")
			}
			if strings.Contains(got, "127.0.0.1:8080") {
				t.Fatalf("the violating URL leaked in strip mode:\n%q", got)
			}
			if !strings.Contains(got, exfilStripPlaceholder) {
				t.Fatalf("placeholder missing in strip mode:\n%q", got)
			}

			blockGuard := NewExfilGuard(config.ExfilGuardConfig{Action: config.ExfilActionBlock}, nil)
			got, blocked = feedGuard(blockGuard, exfilAuditContent, chunkSize)
			if !blocked {
				t.Fatal("block guard did not fire")
			}
			if strings.Contains(got, "127.0.0.1:8080") {
				t.Fatalf("the violating URL leaked in block mode:\n%q", got)
			}

			got, _ = feedGuard(logGuard(), exfilAuditContent, chunkSize)
			if got != exfilAuditContent {
				t.Fatalf("log mode corrupted content:\n%q", got)
			}
		})
	}
}

// TestExfilGuardAltTextWhitespaceHeldWhole pins the crossing-span rule:
// a markdown construct whose alt text contains whitespace is only
// emitted complete (its interior whitespace must not talk the hold cut
// into streaming the construct head ahead of the URL).
func TestExfilGuardAltTextWhitespaceHeldWhole(t *testing.T) {
	content := "see ![the docs](http://127.0.0.1:8080/x?d=AAAA) end"
	for _, chunkSize := range []int{1, 2, 3, 5, 7, 11} {
		got, _ := feedGuard(stripGuard(), content, chunkSize)
		if strings.Contains(got, "127.0.0.1:8080") {
			t.Fatalf("chunk %d: the violating URL leaked:\n%q", chunkSize, got)
		}
		if !strings.Contains(got, exfilStripPlaceholder) {
			t.Fatalf("chunk %d: placeholder missing:\n%q", chunkSize, got)
		}
		if !strings.HasSuffix(got, " end") {
			t.Fatalf("chunk %d: trailing prose lost:\n%q", chunkSize, got)
		}
	}
}

// TestExfilGuardEOFTerminatedBareURL pins the Flush terminator rule: a
// bare URL running to the very end of the stream has no in-band
// terminator; EOF stands in for one at Flush, so the tail cannot dodge
// the policy by never being punctuated.
func TestExfilGuardEOFTerminatedBareURL(t *testing.T) {
	content := "see http://127.0.0.1:8080/steal"
	for _, chunkSize := range []int{1, 4, 9} {
		got, _ := feedGuard(stripGuard(), content, chunkSize)
		if strings.Contains(got, "127.0.0.1:8080") {
			t.Fatalf("chunk %d: the EOF-terminated URL leaked:\n%q", chunkSize, got)
		}
		if !strings.Contains(got, exfilStripPlaceholder) {
			t.Fatalf("chunk %d: placeholder missing:\n%q", chunkSize, got)
		}
		// AUD-03: the Flush span mapping previously shifted every span one
		// byte left (prevWindowBytes=1 claims the first content byte is
		// already-emitted text), so the construct's FINAL byte escaped the
		// placeholder. The stream must end exactly at the placeholder.
		if want := "see " + exfilStripPlaceholder; got != want {
			t.Fatalf("chunk %d: output must end exactly at the placeholder:\nwant %q\ngot  %q", chunkSize, want, got)
		}
	}
}

// TestInspectFullEOFTerminatedBareURL pins the buffered-path terminator rule
// (AUD-03): a bare violating URL at the very end of a non-streaming surface
// has no in-band terminator; the EOF padding stands in for one, so it is
// stripped like any other construct. Pre-fix the URL was delivered verbatim
// with ActionPass.
func TestInspectFullEOFTerminatedBareURL(t *testing.T) {
	got, action, _ := stripGuard().InspectFull("see http://127.0.0.1:8080/steal")
	if action != ActionRedact {
		t.Fatalf("an EOF-terminated bare URL must be redacted on the buffered path, got %v", action)
	}
	if want := "see " + exfilStripPlaceholder; got != want {
		t.Fatalf("output must end exactly at the placeholder:\nwant %q\ngot  %q", want, got)
	}

	// Control: the same URL with an in-band terminator strips identically.
	got2, action2, _ := stripGuard().InspectFull("see http://127.0.0.1:8080/steal ok")
	if action2 != ActionRedact {
		t.Fatalf("an in-band-terminated URL must be redacted, got %v", action2)
	}
	if want := "see " + exfilStripPlaceholder + " ok"; got2 != want {
		t.Fatalf("in-band-terminated strip corrupted:\nwant %q\ngot  %q", want, got2)
	}
}

// TestExfilGuardForceEmitBoundedAndClean pins the force floor: on a
// whitespace-free stream (minified JSON) the hold cannot wait for
// whitespace, so the oldest runes force-emit with at most the last
// windowSize runes withheld. Benign content must arrive byte-identical;
// a violating URL crossing the force boundary must splice (possibly as
// two placeholders) rather than leak its head.
func TestExfilGuardForceEmitBoundedAndClean(t *testing.T) {
	benign := strings.Repeat("x", 2*exfilWindowRunes+100)
	got, _ := feedGuard(stripGuard(), benign, 64)
	if got != benign {
		t.Fatalf("benign whitespace-free stream corrupted through the force path:\n%q", got[:120])
	}

	violating := strings.Repeat("y", exfilWindowRunes+2000) + "http://127.0.0.1:8080/x?d=" + strings.Repeat("A", 300) + strings.Repeat("z", 500)
	got, _ = feedGuard(stripGuard(), violating, 97)
	if strings.Contains(got, "127.0.0.1:8080") {
		t.Fatalf("the violating URL leaked across the force boundary:\n%q", got[max(0, len(got)-400):])
	}
	if !strings.Contains(got, exfilStripPlaceholder) {
		t.Fatalf("placeholder missing on the force path:\n%q", got[max(0, len(got)-400):])
	}
}

// wireText concatenates the client-visible model text from the reader's
// wire output: every data frame's model-text field, in order.
func wireText(t *testing.T, out string) string {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		value, isData := SSEDataField([]byte(line))
		if !isData {
			continue
		}
		trimmed := strings.TrimSpace(string(value))
		if trimmed == "" || trimmed == "[DONE]" {
			continue
		}
		parsed := ParseSSEChunk([]byte(trimmed))
		if parsed == nil {
			continue
		}
		b.WriteString(extractExfilContent(parsed))
	}
	return b.String()
}

// TestExfilGuardHeldTailFlushedBeforeDone pins the reader-side terminal
// flush end to end: a URL split across two frames is held, spliced, and
// the client-visible text carries the placeholder BEFORE the [DONE]
// frame, which stays the final frame.
func TestExfilGuardHeldTailFlushedBeforeDone(t *testing.T) {
	guard := stripGuard()
	chunk := strings.Repeat("Z", 300)
	lines := []string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"content":"see ![b](http://"}}]}`,
		`data: {"choices":[{"delta":{"content":"127.0.0.1:8080/x?d=` + chunk + `) done"}}]}`,
		"data: [DONE]",
	}
	src := strings.NewReader(strings.Join(lines, "\n\n") + "\n\n")
	reader := NewSSETransformingReader(src, nil, context.Background())
	reader.SetExfilGuard(guard)
	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("reader: %v", err)
		}
	}
	wire := out.String()

	if text := wireText(t, wire); strings.Contains(text, "127.0.0.1:8080") {
		t.Fatalf("the violating URL leaked on the wire:\n%s", wire)
	}
	if text := wireText(t, wire); !strings.Contains(text, exfilStripPlaceholder) {
		t.Fatalf("expected the placeholder in the client-visible text:\n%s", wire)
	}
	doneIdx := strings.LastIndex(wire, "data: [DONE]")
	phIdx := strings.Index(wire, exfilStripPlaceholder)
	if doneIdx < 0 || phIdx < 0 || phIdx > doneIdx {
		t.Fatalf("expected the placeholder before [DONE], got:\n%s", wire)
	}
	if !strings.HasSuffix(strings.TrimSpace(wire), "data: [DONE]") {
		t.Fatalf("expected [DONE] as the final frame, got:\n%s", wire)
	}
}

// TestExfilGuardBenignStreamNoDuplicationNoLoss pins the benign-wire
// contract the first review round caught broken: with the guard enabled,
// a plain response must reach the client byte-identical — the hold must
// neither duplicate the tail (the reader forwarding originals ahead of
// the flush) nor drop it.
func TestExfilGuardBenignStreamNoDuplicationNoLoss(t *testing.T) {
	guard := stripGuard()
	lines := []string{
		`data: {"choices":[{"delta":{"content":"Hello "}}]}`,
		`data: {"choices":[{"delta":{"content":"world"}}]}`,
		`data: {"choices":[{"delta":{"content":"!"}}]}`,
		"data: [DONE]",
	}
	src := strings.NewReader(strings.Join(lines, "\n\n") + "\n\n")
	reader := NewSSETransformingReader(src, nil, context.Background())
	reader.SetExfilGuard(guard)
	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("reader: %v", err)
		}
	}
	if text := wireText(t, out.String()); text != "Hello world!" {
		t.Fatalf("benign stream corrupted: %q", text)
	}
}

// TestExfilGuardFlushShapeFollowsSurface pins the format-shaping rule:
// an Anthropic-format stream's terminal flush is a content_block_delta
// (text_delta), not an OpenAI delta — the synthesized chunk is
// format-identical to the stream that carried it.
func TestExfilGuardFlushShapeFollowsSurface(t *testing.T) {
	guard := stripGuard()
	lines := []string{
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"see ![b](http://"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"127.0.0.1:8080/x?d=AAAA)"}}`,
		"data: [DONE]",
	}
	src := strings.NewReader(strings.Join(lines, "\n\n") + "\n\n")
	reader := NewSSETransformingReader(src, nil, context.Background())
	reader.SetExfilGuard(guard)
	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("reader: %v", err)
		}
	}
	wire := out.String()
	if text := wireText(t, wire); strings.Contains(text, "127.0.0.1:8080") {
		t.Fatalf("the violating URL leaked on the wire:\n%s", wire)
	}
	if !strings.Contains(wire, `"text_delta"`) || !strings.Contains(wire, `"content_block_delta"`) {
		t.Fatalf("the flush frame must follow the Anthropic surface:\n%s", wire)
	}
	if strings.Contains(wire, `"choices"`) {
		t.Fatalf("an Anthropic stream must not receive an OpenAI-shaped frame:\n%s", wire)
	}
}

// TestStreamFilterEveryOffsetHoldsSecrets pins the StreamFilter contract
// that holds today: across every chunking, the contiguous secret never
// reaches the client, the redaction marker appears, and a straddling
// block pattern fires. (The filter does not yet withhold partial
// matches: a secret's PREFIX characters can precede the redaction — the
// ExfilGuard hold design covers that channel first.)
func TestStreamFilterEveryOffsetHoldsSecrets(t *testing.T) {
	secret := "AKIAIOSFODNN7EXAMPLE"
	content := "key: 密" + secret + "!🎉 end"
	secretRe := regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	blockRe := regexp.MustCompile(`BLOCK[0-9]+`)

	total := len([]rune(content))
	for chunkSize := 1; chunkSize <= total; chunkSize++ {
		t.Run("chunk"+strconv.Itoa(chunkSize), func(t *testing.T) {
			f := NewStreamFilter([]*regexp.Regexp{secretRe}, nil, "[REDACTED]", 256)
			runes := []rune(content)
			var out strings.Builder
			redacted := false
			for start := 0; start < len(runes); {
				end := start + chunkSize
				if end > len(runes) {
					end = len(runes)
				}
				got, action, _ := f.FilterContent(string(runes[start:end]))
				if action == ActionRedact {
					redacted = true
				}
				out.WriteString(got)
				start = end
			}
			if strings.Contains(out.String(), secret) {
				t.Fatalf("chunk %d: the contiguous secret leaked under redaction:\n%q", chunkSize, out.String())
			}
			if !redacted {
				t.Fatalf("chunk %d: expected at least one ActionRedact", chunkSize)
			}
			if !strings.Contains(out.String(), "[REDACTED]") {
				t.Fatalf("chunk %d: expected the redaction marker, got %q", chunkSize, out.String())
			}

			blockContent := "code 密BLOCK" + "42🎉"
			fb := NewStreamFilter(nil, []*regexp.Regexp{blockRe}, "", 256)
			blocked := false
			rb := []rune(blockContent)
			for start := 0; start < len(rb); {
				end := start + chunkSize
				if end > len(rb) {
					end = len(rb)
				}
				if _, action, _ := fb.FilterContent(string(rb[start:end])); action == ActionBlock {
					blocked = true
				}
				start = end
			}
			if !blocked {
				t.Fatalf("chunk %d: the straddling block pattern never fired", chunkSize)
			}
		})
	}
}

// TestExfilGuardFlushBlockModeEOF pins the block-mode Flush contract: a
// bare URL running to the end of the stream is evaluated at Flush with
// EOF standing in for its terminator — in block mode that evaluation
// flips the guard and Flush returns NOTHING (the tail is the violating
// construct; delivering it with a Pass would defeat the policy).
func TestExfilGuardFlushBlockModeEOF(t *testing.T) {
	content := "see http://127.0.0.1:8080/steal"
	for _, chunkSize := range []int{1, 4, 9} {
		guard := NewExfilGuard(config.ExfilGuardConfig{Action: config.ExfilActionBlock}, nil)
		runes := []rune(content)
		var out strings.Builder
		for start := 0; start < len(runes); {
			end := start + chunkSize
			if end > len(runes) {
				end = len(runes)
			}
			got, action, _ := guard.FilterContent(string(runes[start:end]))
			if action == ActionBlock {
				t.Fatalf("chunk %d: block fired in-stream but the URL is unterminated there", chunkSize)
			}
			out.WriteString(got)
			start = end
		}
		held, action := guard.Flush()
		if action != ActionBlock {
			t.Fatalf("chunk %d: Flush must block the EOF-terminated violating URL, got %v (%q)", chunkSize, action, held)
		}
		if held != "" {
			t.Fatalf("chunk %d: Flush must deliver nothing on a block verdict, got %q", chunkSize, held)
		}
	}
}

// TestExfilGuardForceEmitMultiByteIdentity pins the rune-aligned force
// floor: a whitespace-free multi-byte stream (every rune 3 bytes) must
// survive the force path byte-identical — a byte/rune floor mix would
// split runes into U+FFFD.
func TestExfilGuardForceEmitMultiByteIdentity(t *testing.T) {
	benign := strings.Repeat("密", exfilWindowRunes+1000)
	got, _ := feedGuard(stripGuard(), benign, 97)
	if got != benign {
		t.Fatalf("multi-byte whitespace-free stream corrupted through the force path (len %d vs %d)",
			len(got), len(benign))
	}
	if !utf8.ValidString(got) {
		t.Fatal("invalid UTF-8 emitted through the force path")
	}
}
