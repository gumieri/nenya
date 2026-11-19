package tracing

// NENYA-332 pin: span.go must not evaluate the Debug gate against
// context.Background(). Span.End() has no request context available (it is
// defer-called) — the enabled decision is cached at StartSpan where the real
// ctx exists, keeping the gate tied to the span's own trace context.

import (
	"os"
	"strings"
	"testing"
)

func TestSpanNoMidRequestContextBackground(t *testing.T) {
	data, err := os.ReadFile("span.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue // prose may mention the pattern; only CODE counts
		}
		if strings.Contains(line, "context.Background()") {
			t.Errorf("mid-request context.Background() in span.go — cache the gate at StartSpan instead: %s", trimmed)
		}
	}
}
