package stream

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/nenya/config"
)

// AUD-04: the stream-side egress filters (ExfilGuard, canary tripwire,
// StreamFilter, entropy) extracted model text from OpenAI and Anthropic
// shapes only — a Gemini-native upstream (candidates[].content.parts[].text,
// scanned raw before the provider transformer converts it) streamed every
// filter-blind. These tests pin the Gemini shape on all of them.

const geminiViolatingChunk = `data: {"candidates":[{"content":{"parts":[{"text":"see http://127.0.0.1:8080/steal"}]}}]}` + "\n\n"

func drainGemini(t *testing.T, wire string, setup func(*SSETransformingReader)) string {
	t.Helper()
	r := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	if setup != nil {
		setup(r)
	}
	out, err := io.ReadAll(r)
	if err != nil && err != io.EOF {
		t.Fatalf("read: %v", err)
	}
	return string(out)
}

// A violating URL in a Gemini text part must be withheld and flushed as a
// placeholder at the terminal — never streamed verbatim.
func TestGeminiNativeExfilGuardStripsURL(t *testing.T) {
	wire := geminiViolatingChunk + "data: [DONE]\n\n"
	out := drainGemini(t, wire, func(r *SSETransformingReader) { r.SetExfilGuard(stripGuard()) })

	if strings.Contains(out, "127.0.0.1:8080") {
		t.Fatalf("the violating URL leaked on a Gemini-native stream:\n%s", out)
	}
	if !strings.Contains(out, "EXFIL LINK REMOVED") {
		t.Fatalf("the withheld URL must reach the client as a placeholder:\n%s", out)
	}
}

// An in-band (whitespace-terminated) violating URL in a Gemini text part is
// stripped in place, keeping the surrounding prose.
func TestGeminiNativeExfilGuardStripsInBandURL(t *testing.T) {
	wire := `data: {"candidates":[{"content":{"parts":[{"text":"see http://127.0.0.1:8080/steal now"}]}}]}` + "\n\ndata: [DONE]\n\n"
	out := drainGemini(t, wire, func(r *SSETransformingReader) { r.SetExfilGuard(stripGuard()) })

	if strings.Contains(out, "127.0.0.1:8080") {
		t.Fatalf("the violating URL leaked on a Gemini-native stream:\n%s", out)
	}
	if !strings.Contains(out, "EXFIL LINK REMOVED") || !strings.Contains(out, "now") {
		t.Fatalf("the in-band strip must keep the prose:\n%s", out)
	}
}

// The held tail flushes in the Gemini surface shape (a parts[].text chunk),
// never an OpenAI frame.
func TestGeminiNativeHeldTailFlushesGeminiShaped(t *testing.T) {
	out := drainGemini(t, geminiViolatingChunk+"data: [DONE]\n\n", func(r *SSETransformingReader) { r.SetExfilGuard(stripGuard()) })

	ph := strings.Index(out, "EXFIL LINK REMOVED")
	if ph == -1 {
		t.Fatalf("placeholder missing:\n%s", out)
	}
	frame := out[max(0, ph-200):ph]
	if !strings.Contains(frame, `"parts"`) || !strings.Contains(frame, `"text"`) {
		t.Fatalf("the flush frame must follow the Gemini surface:\n%s", out)
	}
	if strings.Contains(out, `"choices"`) {
		t.Fatalf("a Gemini stream must not receive an OpenAI-shaped flush:\n%s", out)
	}
}

// The canary tripwire must fire on a Gemini-native part: a reproduced canary
// token blocks the stream (ErrCanaryDetected) instead of riding through.
func TestGeminiNativeCanaryBlocked(t *testing.T) {
	const canary = "NENYA-CANARY-0123456789abcdef0123456789abcdef"
	wire := `data: {"candidates":[{"content":{"parts":[{"text":"leaking ` + canary + ` now"}]}}]}` + "\n\n"
	r := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	r.SetCanaryWatcher(NewCanaryWatcher(canary, config.CanaryActionBlock, "stream", nil, nil))

	out, err := io.ReadAll(r)
	if !errors.Is(err, ErrCanaryDetected) {
		t.Fatalf("a canary reproduced in a Gemini part must block the stream with ErrCanaryDetected, got %v", err)
	}
	if strings.Contains(string(out), canary) {
		t.Fatalf("the canary token itself leaked:\n%s", out)
	}
}

// The secret StreamFilter must redact a matched pattern carried in a
// Gemini-native text part.
func TestGeminiNativeSecretFilterRedacts(t *testing.T) {
	secret := "AKIAIOSFODNN7EXAMPLE"
	wire := `data: {"candidates":[{"content":{"parts":[{"text":"key: ` + secret + ` end"}]}}]}` + "\n\ndata: [DONE]\n\n"
	out := drainGemini(t, wire, func(r *SSETransformingReader) {
		r.SetStreamFilter(NewStreamFilter([]*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)}, nil, "[REDACTED]", 256))
	})

	if strings.Contains(out, secret) {
		t.Fatalf("the secret leaked on a Gemini-native stream:\n%s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected the redaction placeholder:\n%s", out)
	}
}

// A chunk with MULTIPLE text parts (Gemini 2.5/3 thought-summary part next
// to the answer part) must have EVERY part scanned: a violation in a later
// part streams through if only parts[0] is evaluated.
func TestGeminiNativeMultiPartAllScanned(t *testing.T) {
	wire := `data: {"candidates":[{"content":{"parts":[{"text":"thinking out loud"},{"text":"answer http://127.0.0.1:8080/steal now"}]}}]}` + "\n\ndata: [DONE]\n\n"
	out := drainGemini(t, wire, func(r *SSETransformingReader) { r.SetExfilGuard(stripGuard()) })

	if strings.Contains(out, "127.0.0.1:8080") {
		t.Fatalf("a violation in a later text part leaked:\n%s", out)
	}
	// "loud" is a whitespace-free tail: the guard holds it across the part
	// boundary and re-emits it joined to the next part's evaluation, so the
	// prose survives in render order, not per-part contiguously.
	if !strings.Contains(out, "thinking out") || !strings.Contains(out, "now") {
		t.Fatalf("benign part content must survive:\n%s", out)
	}
	if !strings.Contains(out, "EXFIL LINK REMOVED") {
		t.Fatalf("the later-part violation must be stripped:\n%s", out)
	}
}

// A multi-part secret: the StreamFilter scans every part.
func TestGeminiNativeMultiPartSecretRedacted(t *testing.T) {
	secret := "AKIAIOSFODNN7EXAMPLE"
	wire := `data: {"candidates":[{"content":{"parts":[{"text":"plan"},{"text":"key: ` + secret + `"}]}}]}` + "\n\ndata: [DONE]\n\n"
	out := drainGemini(t, wire, func(r *SSETransformingReader) {
		r.SetStreamFilter(NewStreamFilter([]*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)}, nil, "[REDACTED]", 256))
	})

	if strings.Contains(out, secret) {
		t.Fatalf("a secret in a later text part leaked:\n%s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected the redaction placeholder:\n%s", out)
	}
}

// A non-text part (functionCall) before the text part must not hide the
// text from the filters.
func TestGeminiNativeNonTextPartSkipped(t *testing.T) {
	secret := "AKIAIOSFODNN7EXAMPLE"
	wire := `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":"x"}},{"text":"key: ` + secret + `"}]}}]}` + "\n\ndata: [DONE]\n\n"
	out := drainGemini(t, wire, func(r *SSETransformingReader) {
		r.SetStreamFilter(NewStreamFilter([]*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)}, nil, "[REDACTED]", 256))
	})

	if strings.Contains(out, secret) {
		t.Fatalf("the secret leaked past a non-text part:\n%s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected the redaction placeholder:\n%s", out)
	}
}

// A hybrid chunk (textless OpenAI delta next to Gemini candidates) must
// flush its held tail in the GEMINI surface: the surface is claimed by the
// shape the text was actually read from, not by whichever shape appears
// first in the chunk.
func TestGeminiNativeHybridChunkFlushesGeminiSurface(t *testing.T) {
	wire := `data: {"choices":[{"index":0,"delta":{}}],"candidates":[{"content":{"parts":[{"text":"see http://127.0.0.1:8080/steal"}]}}]}` + "\n\ndata: [DONE]\n\n"
	out := drainGemini(t, wire, func(r *SSETransformingReader) { r.SetExfilGuard(stripGuard()) })

	if strings.Contains(out, "127.0.0.1:8080") {
		t.Fatalf("the violating URL leaked:\n%s", out)
	}
	ph := strings.Index(out, "EXFIL LINK REMOVED")
	if ph == -1 {
		t.Fatalf("placeholder missing:\n%s", out)
	}
	frame := out[max(0, ph-200):ph]
	if !strings.Contains(frame, `"parts"`) {
		t.Fatalf("the flush frame must follow the Gemini surface the text was read from:\n%s", out)
	}
}

// The Anthropic-native delta (content_block_delta top-level delta) is one of
// the surfaces forEachFilterableTextPart visits: a secret riding an
// Anthropic passthrough chunk is redacted like its OpenAI counterpart.
func TestAnthropicNativeSecretFilterRedacted(t *testing.T) {
	secret := "AKIAIOSFODNN7EXAMPLE"
	wire := `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"key: ` + secret + `"}}` + "\n\ndata: [DONE]\n\n"
	out := drainGemini(t, wire, func(r *SSETransformingReader) {
		r.SetStreamFilter(NewStreamFilter([]*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)}, nil, "[REDACTED]", 256))
	})

	if strings.Contains(out, secret) {
		t.Fatalf("the secret leaked on an Anthropic-native stream:\n%s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected the redaction placeholder:\n%s", out)
	}
}
