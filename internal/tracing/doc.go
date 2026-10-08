// Package tracing provides OTel-lite request tracing over the standard
// library only: W3C Trace Context propagation (traceparent parse/format),
// context-derived stage spans, and log-based export via slog — no
// OpenTelemetry SDK dependency. A trace is a named stage list; every
// span's End emits one structured slog record and the request summary
// lands at Info level, so existing log pipelines collect traces without
// new infrastructure. Stdlib-only leaf: imported by the pipeline and
// proxy; it must not import any other repository package.
package tracing
