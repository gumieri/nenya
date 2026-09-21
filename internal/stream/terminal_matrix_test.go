package stream

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// drainReader reads the reader to EOF and returns the full output.
func drainReader(t *testing.T, r io.Reader) string {
	t.Helper()
	out, err := io.ReadAll(r)
	if err != nil && err != io.EOF {
		t.Fatalf("unexpected read error: %v", err)
	}
	return string(out)
}

// TestSynthesizeFinishBeforeDone pins the [DONE]-without-finish_reason cell
// of the terminal-state matrix (NENYA-46): an OpenAI passthrough stream that
// streamed content but never signaled completion gets a synthetic
// finish_reason:"stop" chunk emitted ahead of [DONE], closing the client
// message protocol-correctly.
func TestSynthesizeFinishBeforeDone(t *testing.T) {
	input := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: [DONE]\n\n"
	reader := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())

	out := drainReader(t, reader)

	synthIdx := strings.Index(out, `"finish_reason":"stop"`)
	doneIdx := strings.Index(out, "data: [DONE]")
	if synthIdx < 0 {
		t.Fatalf("expected synthesized finish_reason chunk in output, got %q", out)
	}
	if doneIdx < 0 {
		t.Fatal("expected [DONE] in output")
	}
	if synthIdx > doneIdx {
		t.Fatalf("synthesized finish must precede [DONE], got %q", out)
	}
	if !reader.SawDone() {
		t.Error("SawDone must be true")
	}
	if !reader.SawFinishReason() {
		t.Error("SawFinishReason must be true after synthesis")
	}
	if reader.InjectedError() {
		t.Error("synthesis is not an error; InjectedError must stay false")
	}
}

// TestNoSynthesisWhenFinishSeen checks the healthy flow is untouched: a real
// finish_reason chunk means [DONE] passes through without synthesis.
func TestNoSynthesisWhenFinishSeen(t *testing.T) {
	input := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	reader := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())

	out := drainReader(t, reader)

	if strings.Count(out, `"finish_reason"`) != 1 {
		t.Errorf("expected exactly the original finish_reason, got %q", out)
	}
	if strings.Count(out, "data: ") != 3 {
		t.Errorf("expected exactly content+finish+[DONE] lines (no synthesis), got %q", out)
	}
	if !reader.SawFinishReason() {
		t.Error("SawFinishReason must be true from the real finish chunk")
	}
}

// TestNoSynthesisWithoutContent checks streams that never produced content
// (usage-only, keepalive-only) end exactly as before.
func TestNoSynthesisWithoutContent(t *testing.T) {
	input := "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":null}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":0,\"total_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"
	reader := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())

	out := drainReader(t, reader)

	if strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("no synthesis expected without streamed content: %q", out)
	}
}

// TestNoSynthesisWithTransformer checks format-converting streams are left to
// their transformer: the client format is unknown to the generic synthesis.
func TestNoSynthesisWithTransformer(t *testing.T) {
	input := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"
	reader := NewSSETransformingReader(strings.NewReader(input), passthroughTransformer{}, context.Background())

	out := drainReader(t, reader)

	if strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("synthesis must not fire when a transformer owns the format: %q", out)
	}
	if !reader.SawDone() {
		t.Error("SawDone must be true")
	}
}

// TestNoSynthesisDuringDiscard checks discard mode (size-limit shedding)
// keeps the plain [DONE] passthrough without extra frames — the injected
// error terminal preempts any line-level synthesis.
func TestNoSynthesisDuringDiscard(t *testing.T) {
	input := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + strings.Repeat("x", 200) + "\"}}]}\n\n" +
		"data: [DONE]\n\n"
	reader := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
	reader.SetMaxTransformedBytes(64)

	out, err := io.ReadAll(reader)
	if err != nil && !errors.Is(err, ErrTransformedSizeExceeded) {
		t.Fatalf("unexpected read error: %v", err)
	}
	got := string(out)

	if !strings.Contains(got, "data: [DONE]") {
		t.Errorf("discard mode must still pass [DONE]: %q", got)
	}
	if strings.Contains(got, `"finish_reason":"stop"`) {
		t.Errorf("synthesis must not fire in discard mode: %q", got)
	}
}

// TestPendingToolCallArgsNotContent checks that argument chunks buffered
// while waiting for a name do not count as streamed content (no synthesis
// fires for them) and the stream ends cleanly with the loss telemetry path
// exercised.
func TestPendingToolCallArgsNotContent(t *testing.T) {
	input := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"\",\"function\":{\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	reader := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())

	out := drainReader(t, reader)

	if strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("buffered tool args are not content; no synthesis expected: %q", out)
	}
	if strings.Contains(out, `"a":`) {
		t.Errorf("nameless argument chunks must stay buffered (not forwarded): %q", out)
	}
	if !reader.SawDone() {
		t.Error("SawDone must be true")
	}
}
