// Package pipeline implements the request processing pipeline applied to
// outbound payloads before they reach upstream providers. Stages include
// secret redaction (pattern-based and entropy-based), whitespace compaction,
// stale tool-call and thought pruning, TF-IDF relevance scoring, middle-out
// truncation, window-based conversation compaction via engine summarization,
// and engine chain invocation with fallback.
package pipeline
