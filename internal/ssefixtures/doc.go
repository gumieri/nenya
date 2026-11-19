// Package ssefixtures provides deterministic, production-shaped SSE wire
// fixtures and chat payloads for benchmarks (NENYA-337 follow-up, work item
// 5a8943f0): the hot-path benches (transformer, accumulator, transform)
// measure against the shapes real clients and upstreams send — tool-call
// argument deltas split across frames, usage chunks, thinking blocks — not
// against one-line synthetic streams.
//
// Every builder is deterministic (no randomness) so benchstat runs are
// comparable, and every fixture is validated by the Test*Sanity functions
// in this package: the corpus cannot drift into unparseable shapes silently.
package ssefixtures
