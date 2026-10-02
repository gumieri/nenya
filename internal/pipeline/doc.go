// Package pipeline implements the request processing pipeline applied to
// outbound payloads before they reach upstream providers. Stages include
// secret redaction (pattern-based and entropy-based), whitespace compaction,
// stale tool-call and thought pruning, TF-IDF relevance scoring, middle-out
// truncation, window-based conversation compaction via engine summarization,
// and engine chain invocation with fallback.
//
// The package also houses the typed advisory-judgment layer: code-owned
// judgment contracts (Judge) drive bounded, fail-closed LLM adjudications at
// ambiguous bands and buffered checkpoints only. The layer is engine-agnostic
// (chat or System One decision models), can only strengthen a deterministic
// tier-1 verdict, and falls back to that verdict on any operational failure,
// contract violation, or truncated excerpt. See judge.go and engine.go.
package pipeline
