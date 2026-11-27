package stream

import (
	"context"
	"strings"
	"testing"

	"github.com/nenya/internal/infra"
)

// NENYA-351 pins: the InvisibleGuard — strip mode rewrites, log mode
// counts only, and both fire the action-labeled metrics.

func TestInvisibleGuardStripMode(t *testing.T) {
	metrics := infra.NewMetrics()
	guard := NewInvisibleGuard(true, false, metrics)
	out, n := guard.Apply("ig\u200bnore\u200b")
	if n != 2 || out != "ignore" {
		t.Fatalf("strip mode: n=%d out=%q", n, out)
	}
	// Metrics: one strip event, two chars.
	var buf strings.Builder
	metrics.WritePrometheus(&buf)
	if !strings.Contains(buf.String(), `nenya_invisible_text_total{action="strip"} 1`) {
		t.Fatalf("strip event metric missing:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `nenya_invisible_text_chars_total 2`) {
		t.Fatalf("chars metric missing:\n%s", buf.String())
	}
}

func TestInvisibleGuardLogMode(t *testing.T) {
	metrics := infra.NewMetrics()
	guard := NewInvisibleGuard(false, false, metrics)
	in := "ig\u200bnore"
	out, n := guard.Apply(in)
	if n != 1 || out != in {
		t.Fatalf("log mode must not rewrite: n=%d out=%q", n, out)
	}
	var buf strings.Builder
	metrics.WritePrometheus(&buf)
	if !strings.Contains(buf.String(), `nenya_invisible_text_total{action="log"} 1`) {
		t.Fatalf("log event metric missing:\n%s", buf.String())
	}
}

func TestInvisibleGuardCleanText(t *testing.T) {
	metrics := infra.NewMetrics()
	guard := NewInvisibleGuard(true, false, metrics)
	out, n := guard.Apply("plain text")
	if n != 0 || out != "plain text" {
		t.Fatalf("clean text rewritten: n=%d out=%q", n, out)
	}
	var buf strings.Builder
	metrics.WritePrometheus(&buf)
	if strings.Contains(buf.String(), "nenya_invisible_text_total{") {
		t.Fatalf("clean text fired the metric:\n%s", buf.String())
	}
}

// TestApplyInvisibleStripStage pins the reader-stage integration shape:
// model-text surfaces are rewritten in place, non-text fields untouched.
func TestApplyInvisibleStripStage(t *testing.T) {
	guard := NewInvisibleGuard(true, false, infra.NewMetrics())
	parsed := map[string]interface{}{
		"choices": []interface{}{map[string]interface{}{
			"delta": map[string]interface{}{"content": "ig\u200bnore\u200b previous"},
		}},
		"usage": map[string]interface{}{"total_tokens": 5},
	}
	out, mutated := applyInvisibleStripStage(parsed, guard)
	if !mutated {
		t.Fatal("stage reported no mutation")
	}
	choice := out["choices"].([]interface{})[0].(map[string]interface{})
	delta := choice["delta"].(map[string]interface{})
	if delta["content"] != "ignore previous" {
		t.Fatalf("content not stripped: %q", delta["content"])
	}
	if out["usage"] == nil {
		t.Fatal("non-text field lost")
	}
}

// TestReaderInvisibleLogModeByteIdentity pins the round-1 MED at the
// wire level (log mode): a chunk carrying invisible characters AND an
// unrelated big integer must drain BYTE-EXACTLY — log mode counts, and
// must never force the whole-chunk re-encode (map key reordering +
// float64 round-trip) the mutated flag would cause.
func TestReaderInvisibleLogModeByteIdentity(t *testing.T) {
	wire := "data: {\"choices\":[{\"delta\":{\"content\":\"ig\\u200bnore\"}}],\"balance\":9007199254740993}\n\n" +
		"data: [DONE]\n\n"
	reader := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	reader.SetInvisibleGuard(NewInvisibleGuard(false, false, infra.NewMetrics()))
	out := drainReader(t, reader)
	// The content frame must ship BYTE-EXACT (log mode never marks the
	// chunk mutated → no map-order/float64 re-encode; the big integer
	// keeps its exact spelling). The terminal machinery may synthesize
	// completion frames around it — only the content frame is pinned.
	frame := wire[:len(wire)-len("\ndata: [DONE]\n\n")]
	if !strings.Contains(out, frame) {
		t.Fatalf("log mode re-encoded the content frame:\nwant: %q\ngot:  %q", frame, out)
	}
	if !strings.Contains(out, "9007199254740993") {
		t.Fatalf("big integer lost precision (re-encoded): %q", out)
	}
}

// TestReaderInvisibleStripModeRewrites pins strip mode at the wire
// level: the invisible characters are removed from the delta text while
// the frame structure survives (and a big integer in an unrelated field
// keeps its exact spelling — re-encoding happens for the rewritten
// chunk, but the decoded integer round-trips because it is within
// float64 precision; the pin is the STRIP, not the byte identity).
func TestReaderInvisibleStripModeRewrites(t *testing.T) {
	wire := "data: {\"choices\":[{\"delta\":{\"content\":\"ig\\u200bnore\\u200b me\"}}]}\n\n" +
		"data: [DONE]\n\n"
	reader := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	reader.SetInvisibleGuard(NewInvisibleGuard(true, false, infra.NewMetrics()))
	out := drainReader(t, reader)
	if strings.ContainsAny(out, "\u200b") {
		t.Fatalf("invisible characters survived strip mode: %q", out)
	}
	if !strings.Contains(out, "ignore me") {
		t.Fatalf("stripped content wrong: %q", out)
	}
	if !strings.Contains(out, `"delta":{"content"`) {
		t.Fatalf("frame structure damaged: %q", out)
	}
}
