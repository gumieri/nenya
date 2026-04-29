package util

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDoWithRetry_SucceedsFirstAttempt(t *testing.T) {
	var calls int
	err := DoWithRetry(context.Background(), 3, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestDoWithRetry_SucceedsAfterFailures(t *testing.T) {
	var calls int
	err := DoWithRetry(context.Background(), 3, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient error")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestDoWithRetry_Exhausted(t *testing.T) {
	expected := errors.New("permanent error")
	var calls int
	err := DoWithRetry(context.Background(), 3, func() error {
		calls++
		return expected
	})
	if !errors.Is(err, expected) {
		t.Fatalf("expected %v, got %v", expected, err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestDoWithRetry_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := DoWithRetry(ctx, 3, func() error {
		return errors.New("some error")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestDoWithRetry_MaxAttemptsOne(t *testing.T) {
	var calls int
	err := DoWithRetry(context.Background(), 1, func() error {
		calls++
		return errors.New("fail")
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestDoWithRetry_ZeroAttempts(t *testing.T) {
	var calls int
	err := DoWithRetry(context.Background(), 0, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call (fallback to single), got %d", calls)
	}
}

func TestCalculateBackoff_Increases(t *testing.T) {
	d0 := CalculateBackoff(0)
	d1 := CalculateBackoff(1)
	d2 := CalculateBackoff(2)
	if d0 >= d1 {
		t.Errorf("expected backoff(0) < backoff(1), got %v >= %v", d0, d1)
	}
	if d1 >= d2 {
		t.Errorf("expected backoff(1) < backoff(2), got %v >= %v", d1, d2)
	}
}

func TestCalculateBackoff_Capped(t *testing.T) {
	for i := 0; i < 10; i++ {
		d := CalculateBackoff(i)
		if d > exponentialBackoffMax+exponentialBackoffJitter {
			t.Errorf("backoff(%d) = %v exceeds max %v", i, d, exponentialBackoffMax+exponentialBackoffJitter)
		}
	}
}

func TestCalculateBackoff_NonNegative(t *testing.T) {
	for i := 0; i < 20; i++ {
		d := CalculateBackoff(i)
		if d < 0 {
			t.Errorf("backoff(%d) = %v is negative", i, d)
		}
	}
}

func TestDefaultMaxRetryAttempts(t *testing.T) {
	if n := DefaultMaxRetryAttempts(); n != 3 {
		t.Fatalf("expected 3, got %d", n)
	}
}

func TestDoWithRetry_ContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := DoWithRetry(ctx, 10, func() error {
		return errors.New("keep failing")
	})
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}
