package stream

import (
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
)

// InvisibleGuard is the outbound invisible-text guard (NENYA-351):
// hidden/format characters in model output are counted (log action) or
// stripped (strip action) before the downstream consumer sees them.
// Character-level mutation needs no hold-back — SSE frames carry
// complete JSON strings, so every invisible rune arrives whole within a
// frame (unlike the URL constructs the ExfilGuard's window chases).
// Instances are per-response and not safe for concurrent use, like the
// other guards.
type InvisibleGuard struct {
	strip    bool // false = log/count only
	extended bool // the extended ignorable set (variation selectors etc.)
	metrics  *infra.Metrics
}

// NewInvisibleGuard builds the guard from the governance.invisible_text
// config.
func NewInvisibleGuard(strip, extended bool, metrics *infra.Metrics) *InvisibleGuard {
	return &InvisibleGuard{strip: strip, extended: extended, metrics: metrics}
}

// Apply processes one text surface: strip mode removes the configured
// invisible set and returns the rewritten text; log mode returns the
// text UNCHANGED (counting only). Both count removed characters (the
// chars metric) and fire the event metric exactly once per non-empty
// surface.
func (g *InvisibleGuard) Apply(content string) (string, int) {
	stripped, n := pipeline.StripInvisibleForOutput(content, g.extended)
	if n == 0 {
		return content, 0
	}
	action := "log"
	if g.strip {
		action = "strip"
	}
	g.metrics.RecordInvisibleText(action, n)
	if !g.strip {
		return content, n
	}
	return stripped, n
}
