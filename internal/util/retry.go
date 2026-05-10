// Package util provides shared utility functions for common operations
// including retry logic with exponential backoff, integer overflow
// protection, and string formatting.
//
// The retry helper (DoWithRetry) is used throughout the codebase
// to handle transient network errors and server-side failures.
package util

import (
	"context"
	"math/rand"
	"time"
)

const (
	defaultMaxRetryAttempts  = 3
	exponentialBackoffMax    = 8 * time.Second
	exponentialBackoffBase   = 500 * time.Millisecond
	exponentialBackoffJitter = 750 * time.Millisecond
)

// CalculateBackoff returns the backoff duration for the given attempt number
// (0-indexed). Uses exponential backoff with jitter, capped at 8s.
//
// The jitter prevents thundering herd when multiple retries are scheduled
// simultaneously.
func CalculateBackoff(attempt int) time.Duration {
	delay := exponentialBackoffBase
	for i := 0; i < attempt; i++ {
		delay *= 2
		if delay >= exponentialBackoffMax {
			delay = exponentialBackoffMax
			break
		}
	}
	jitter := time.Duration(rand.Int63n(int64(exponentialBackoffJitter)))
	return delay + jitter
}

// DoWithRetry calls fn up to maxAttempts times. Returns nil on the first
// successful call (fn returns nil). Returns the last error if all attempts
// fail. Respects ctx cancellation during backoff waits.
//
// If maxAttempts <= 1, fn is called once with no retry.
//
// The function uses exponential backoff with jitter for retries:
// - Attempt 0: no delay (immediate)
// - Attempt 1: 500-1250ms
// - Attempt 2: 1000-2500ms
// - Attempt 3+: 2000-2750ms (capped at 8s)
//
// Context cancellation immediately stops retry attempts and returns ctx.Err().
func DoWithRetry(ctx context.Context, maxAttempts int, fn func() error) error {
	if maxAttempts <= 1 {
		return fn()
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := fn(); err != nil {
			lastErr = err
			if attempt == maxAttempts-1 {
				return lastErr
			}
			backoff := CalculateBackoff(attempt)
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
			continue
		}
		return nil
	}
	return lastErr
}

// DefaultMaxRetryAttempts returns the default maximum retry attempts.
func DefaultMaxRetryAttempts() int {
	return defaultMaxRetryAttempts
}
