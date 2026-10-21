package stream

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// The empty-stream callback is the NENYA-160 always-on streaming signal: it
// fires when the scanner ends cleanly with NO content at all (the same
// condition that injects the gateway_error), and never for a normal stream.
func TestSSETransformingReaderEmptyStreamCallback(t *testing.T) {
	var fired atomic.Int64

	// Empty stream: no events at all.
	reader := NewSSETransformingReader(strings.NewReader(""), nil, context.Background())
	reader.SetEmptyStreamCallback(func() { fired.Add(1) })
	for {
		_, err := reader.Read(make([]byte, 512))
		if err != nil {
			break
		}
	}
	if fired.Load() != 1 {
		t.Fatalf("an empty stream must fire the callback exactly once, got %d", fired.Load())
	}

	// A normal stream with content: never.
	fired.Store(0)
	reader = NewSSETransformingReader(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"), nil, context.Background())
	reader.SetEmptyStreamCallback(func() { fired.Add(1) })
	for {
		_, err := reader.Read(make([]byte, 512))
		if err != nil {
			break
		}
	}
	if fired.Load() != 0 {
		t.Fatalf("a stream with content must not fire the callback: %d", fired.Load())
	}

	// No callback set: no panic, same injection behavior.
	reader = NewSSETransformingReader(strings.NewReader(""), nil, context.Background())
	for {
		_, err := reader.Read(make([]byte, 512))
		if err != nil {
			break
		}
	}
}
