package infra

// NENYA-332 pin: the response cache's REQUEST-path methods (Lookup chain)
// must not evaluate debug gates against context.Background() — a mid-request
// context.Background() severs trace propagation and request-scoped values.
// The eviction sweep is a background janitor goroutine (no request scope
// exists); its site carries the background-janitor marker and is exempt.

import (
	"os"
	"strings"
	"testing"
)

func TestResponseCacheRequestPathsNoMidRequestContextBackground(t *testing.T) {
	data, err := os.ReadFile("response_cache.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	violations := 0
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue // prose may mention the pattern; only CODE counts
		}
		if !strings.Contains(line, "context.Background()") {
			continue
		}
		if strings.Contains(line, "background-janitor") {
			continue
		}
		violations++
		t.Errorf("mid-request context.Background() in response_cache.go (use the threaded request ctx, or mark a genuine background goroutine): %s", strings.TrimSpace(line))
	}
	if violations == 0 {
		t.Log("clean: every context.Background() site is marked background-janitor")
	}
}
