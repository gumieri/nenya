package routing

import (
	"strings"
	"testing"

	"github.com/nenya/internal/adapter"
)

// hugeUnmarkedPayload builds a message array whose node count exceeds the
// deep-scan budget (maxNodes = 20000 in hasClientCacheControl) BEFORE a
// client cache_control mark at the tail is reached. The scan therefore ends
// truncated, and whether the request is marked is UNKNOWN.
func hugeUnmarkedPayload() map[string]interface{} {
	filler := make([]interface{}, 0, 22000)
	for i := 0; i < 22000; i++ {
		filler = append(filler, map[string]interface{}{
			"role": "user", "content": strings.Repeat("x", 8),
		})
	}
	filler = append(filler, map[string]interface{}{
		"role":    "user",
		"content": []interface{}{map[string]interface{}{"type": "text", "text": "marked", "cache_control": map[string]interface{}{"type": "ephemeral"}}},
	})
	return map[string]interface{}{"model": "claude-test", "messages": filler}
}

// TestAutoCacheBreakpointsFailClosedOnScanTruncation pins AUD-34
// (NENYA-253): when the client-mark deep scan exhausts its budget, whether
// the client placed a mark is UNKNOWN — the gateway must stand down (no
// injection) and record the outcome, not fail open and anchor over a
// possibly-marked request. Pre-fix the truncated scan returned false ("no
// marks") and the injection proceeded with only a Debug log.
func TestAutoCacheBreakpointsFailClosedOnScanTruncation(t *testing.T) {
	deps, cfg := autoCacheDeps(t, true)
	opts := adapter.CacheOpts{}
	applyAutoCacheBreakpoints(deps, hugeUnmarkedPayload(), cfg.PrefixCache, &opts, false)

	if opts.System || opts.Messages {
		t.Fatalf("truncated scan must fail closed: no anchoring over an unscanned request (opts = %+v)", opts)
	}
	var sb strings.Builder
	deps.Metrics.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `nenya_auto_cache_breakpoints_total{outcome="skipped_scan_truncated"} 1`) {
		t.Fatalf("truncation must be recorded as an outcome:\n%s", sb.String())
	}
}
