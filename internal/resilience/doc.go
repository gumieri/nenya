// Package resilience implements a per-key circuit breaker following the
// Closed -> Open -> HalfOpen state machine. Each agent+provider+model combination
// gets an independent breaker with configurable failure/success thresholds and
// automatic recovery via half-open probing.
package resilience
