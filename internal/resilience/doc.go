// Package resilience implements an enhanced per-key circuit breaker with error-semantic
// cooldowns. Instead of treating all failures equally, different error types receive
// appropriate cooldown durations: authentication errors get long cooldowns (2 min),
// rate limit and quota errors get exponential backoff, capacity errors get shorter
// backoff, and transient server errors get short cooldowns.
//
// Each agent+provider+model combination gets an independent breaker with configurable
// failure/success thresholds and automatic recovery via half-open probing.
//
// Key features:
//   - Error-semantic classification: different HTTP status codes and error messages
//     receive tailored cooldown strategies
//   - Per-model locking: individual models can be locked without affecting others
//   - Exponential backoff with jitter: prevents thundering herd during repeated failures
//   - Thread-safe: all operations are safe for concurrent use
//   - Observability: backoff increments and circuit events are exposed via metrics
package resilience
