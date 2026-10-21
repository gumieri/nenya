package infra

import (
	"strings"
	"sync"
)

// ---- NENYA-144: bounded model metric labels --------------------------------
//
// Model identifiers reach metric labels and per-model /statsz maps from
// several surfaces — including passthrough endpoints (/v1/responses,
// /v1/embeddings, /v1/rerank) whose model strings are validated only by the
// upstream. A hostile client can therefore feed arbitrarily many distinct
// "model" values into sync.Map-backed label storage: unbounded memory and an
// unbounded /metrics series list. The guard bounds both axes:
//
//   - charset/length: a label that is not a plausible model id collapses to
//     the single InvalidModelLabel sentinel;
//   - distinct series: once the guard has seen max distinct labels, every NEW
//     one collapses to the single OverflowModelLabel sentinel.
//
// Both collapses are counted by the caller (Metrics counts
// nenya_model_label_violations_total{kind}; UsageTracker surfaces its own
// label_violations in its Snapshot), so operators see the pressure without
// the cardinality. A registry of legitimate models is bounded by the
// operator's configuration and discovery catalog; DefaultMaxModelSeries is
// sized far above any real catalog.

const (
	// MaxModelLabelChars is the longest model string the guard accepts as
	// itself; anything longer collapses to InvalidModelLabel.
	MaxModelLabelChars = 64
	// DefaultMaxModelSeries is the default distinct-model budget per guard
	// (sized far above any real operator catalog).
	DefaultMaxModelSeries = 512
	// InvalidModelLabel is the sentinel for a model string that fails the
	// charset/length check.
	InvalidModelLabel = "_invalid_"
	// OverflowModelLabel is the sentinel for a valid-looking model that
	// arrives after the guard's distinct-series budget is exhausted.
	OverflowModelLabel = "_overflow_"
	// UnknownModelLabel replaces an empty model string.
	UnknownModelLabel = "unknown"
)

// ModelLabelGuard bounds the cardinality of caller-controlled model labels.
// Label is safe for concurrent use; construct with NewModelLabelGuard (the
// zero value admits nothing and reports every non-plausible input as invalid,
// so nil-safe callers prefer the guard-free sanitize helper).
type ModelLabelGuard struct {
	mu   sync.Mutex
	seen map[string]struct{}
	max  int
}

// NewModelLabelGuard returns a guard admitting up to max distinct labels
// (non-positive max means DefaultMaxModelSeries).
func NewModelLabelGuard(max int) *ModelLabelGuard {
	if max <= 0 {
		max = DefaultMaxModelSeries
	}
	return &ModelLabelGuard{seen: make(map[string]struct{}), max: max}
}

// LabelOutcome reports how a model string was mapped to a label.
type LabelOutcome struct {
	// Label is the bounded label value to use.
	Label string
	// Invalid reports a charset/length violation (collapsed to
	// InvalidModelLabel).
	Invalid bool
	// Overflow reports a valid-looking model that exhausted the
	// distinct-series budget (collapsed to OverflowModelLabel).
	Overflow bool
}

// Label maps a caller-controlled model string to a bounded label value. The
// label is the model's CANONICAL LOWERCASE form: the routing registry resolves
// case-insensitively, so case-variant spellings of one model must not mint
// distinct series (an authenticated client could otherwise exhaust the budget
// by enumerating case variants).
func (g *ModelLabelGuard) Label(model string) LabelOutcome {
	if !PlausibleModelLabel(model) {
		return LabelOutcome{Label: InvalidModelLabel, Invalid: true}
	}
	canonical := strings.ToLower(model)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen == nil {
		g.seen = make(map[string]struct{})
	}
	if _, ok := g.seen[canonical]; ok {
		return LabelOutcome{Label: canonical}
	}
	if len(g.seen) >= g.max {
		return LabelOutcome{Label: OverflowModelLabel, Overflow: true}
	}
	// Clone before retaining: strings.ToLower returns the input unchanged
	// when already lowercase, and the input may be a zero-copy substring of
	// a request body — pinning it here would hold whole body buffers for the
	// process lifetime (the NENYA-198 retention class, one hop earlier than
	// the downstream clone sites).
	g.seen[strings.Clone(canonical)] = struct{}{}
	return LabelOutcome{Label: canonical}
}

// modelLabelCharset is the model-id grammar the guard accepts: letters,
// digits, and the separators real registries use (byte-identical to the
// /statsz text-path statsKeyCharset in internal/proxy/handler.go — keep the
// two grammars aligned). Everything else — control characters, spaces,
// 10 KiB junk — collapses to the invalid sentinel.
const modelLabelCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-/:@+~"

// PlausibleModelLabel reports whether a model string can be used as itself in
// a metric label: non-empty, within the length cap, and composed only of the
// model-id grammar.
func PlausibleModelLabel(model string) bool {
	if model == "" || len(model) > MaxModelLabelChars {
		return false
	}
	return strings.IndexFunc(model, func(r rune) bool {
		return !strings.ContainsRune(modelLabelCharset, r)
	}) < 0
}
