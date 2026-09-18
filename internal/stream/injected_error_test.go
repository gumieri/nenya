package stream

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// TestInjectedError pins the NENYA-27 reader signal: any reader-sanitized
// truncation (missing [DONE], oversize line) marks InjectedError, while clean
// and benignly-truncated endings do not.
func TestInjectedError(t *testing.T) {
	t.Run("clean stream with DONE", func(t *testing.T) {
		reader := NewSSETransformingReader(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"), nil, context.Background())
		out, err := io.Copy(io.Discard, reader)
		if err != nil {
			t.Fatalf("copy: %v", err)
		}
		_ = out
		if reader.InjectedError() {
			t.Error("clean stream must not report InjectedError")
		}
	})
	t.Run("benign truncation after finish_reason", func(t *testing.T) {
		input := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"
		reader := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
		if _, err := io.Copy(io.Discard, reader); err != nil {
			t.Fatalf("copy: %v", err)
		}
		if reader.InjectedError() {
			t.Error("finish_reason + injected silent [DONE] is benign, not an injected error")
		}
	})
	t.Run("mid-generation cut injects and flags", func(t *testing.T) {
		input := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
		reader := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
		var out bytes.Buffer
		if _, err := io.Copy(&out, reader); err != nil {
			t.Fatalf("copy: %v", err)
		}
		if !reader.InjectedError() {
			t.Error("mid-generation cut must report InjectedError")
		}
		if !strings.Contains(out.String(), "gateway_error") {
			t.Error("expected injected gateway_error frame in output")
		}
	})
	t.Run("empty stream injects and flags", func(t *testing.T) {
		reader := NewSSETransformingReader(strings.NewReader(""), nil, context.Background())
		if _, err := io.Copy(io.Discard, reader); err != nil {
			t.Fatalf("copy: %v", err)
		}
		if !reader.InjectedError() {
			t.Error("empty stream must report InjectedError")
		}
	})
	t.Run("reset clears flag", func(t *testing.T) {
		reader := NewSSETransformingReader(strings.NewReader(""), nil, context.Background())
		if _, err := io.Copy(io.Discard, reader); err != nil {
			t.Fatalf("copy: %v", err)
		}
		reader.ResetCounters()
		if reader.InjectedError() {
			t.Error("ResetCounters must clear InjectedError")
		}
	})
}
