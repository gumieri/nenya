package resilience

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCircuitBreaker_SetBackoffIncrementCallback(t *testing.T) {
	cb := NewCircuitBreaker(1, 1, 1, time.Second, nil)

	var callbackInvoked atomic.Bool
	var capturedKey string
	var capturedLevel int

	cb.SetBackoffIncrementCallback(func(key string, level int) {
		callbackInvoked.Store(true)
		capturedKey = key
		capturedLevel = level
	})

	// First increment
	cb.RecordFailureWithStatus("test:gpt4", 429, "rate limit exceeded")

	if !callbackInvoked.Load() {
		t.Error("callback should be invoked on first increment")
	}
	if capturedKey != "test:gpt4" {
		t.Errorf("expected key 'test:gpt4', got '%s'", capturedKey)
	}
	if capturedLevel != 1 {
		t.Errorf("expected level 1, got %d", capturedLevel)
	}

	// Second increment
	callbackInvoked.Store(false)
	cb.RecordFailureWithStatus("test:gpt4", 429, "rate limit exceeded")

	if !callbackInvoked.Load() {
		t.Error("callback should be invoked on second increment")
	}
	if capturedLevel != 2 {
		t.Errorf("expected level 2, got %d", capturedLevel)
	}
}

func TestCircuitBreaker_BackoffCallback_Parallel(t *testing.T) {
	cb := NewCircuitBreaker(5, 1, 1, time.Minute, nil)

	var callbackCount atomic.Int64

	cb.SetBackoffIncrementCallback(func(key string, level int) {
		callbackCount.Add(1)
	})

	var wg sync.WaitGroup
	numGoroutines := 10
	incrementsPerGoroutine := 10

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < incrementsPerGoroutine; j++ {
				model := "model" + string(rune('0'+id%10))
				cb.RecordFailureWithStatus("agent:provider:"+model, 429, "rate limit")
			}
		}(i)
	}

	wg.Wait()

	// Each goroutine increments its model 10 times, so 100 total callbacks
	if callbackCount.Load() != int64(numGoroutines*incrementsPerGoroutine) {
		t.Errorf("expected %d callbacks, got %d", numGoroutines*incrementsPerGoroutine, callbackCount.Load())
	}
}

func TestBackoffTracker_Callback(t *testing.T) {
	var capturedCalls []struct {
		model string
		level int
	}

	bt := NewBackoffTrackerWithCallback(func(model string, level int) {
		capturedCalls = append(capturedCalls, struct {
			model string
			level int
		}{model, level})
	})

	bt.Increment("model1")
	bt.Increment("model1")
	bt.Increment("model2")

	if len(capturedCalls) != 3 {
		t.Errorf("expected 3 callbacks, got %d", len(capturedCalls))
	}

	if capturedCalls[0].model != "model1" || capturedCalls[0].level != 1 {
		t.Errorf("first call mismatch: got model=%s level=%d", capturedCalls[0].model, capturedCalls[0].level)
	}

	if capturedCalls[1].model != "model1" || capturedCalls[1].level != 2 {
		t.Errorf("second call mismatch: got model=%s level=%d", capturedCalls[1].model, capturedCalls[1].level)
	}

	if capturedCalls[2].model != "model2" || capturedCalls[2].level != 1 {
		t.Errorf("third call mismatch: got model=%s level=%d", capturedCalls[2].model, capturedCalls[2].level)
	}
}

func TestBackoffTracker_ResetAfterCallback(t *testing.T) {
	bt := NewBackoffTrackerWithCallback(func(model string, level int) {})

	bt.Increment("model1")
	bt.Increment("model1")

	bt.Reset("model1")

	// After reset, increment should start from level 1 again
	bt.Increment("model1")
	if bt.GetLevel("model1") != 1 {
		t.Errorf("expected level 1 after reset, got %d", bt.GetLevel("model1"))
	}
}

func TestBackoffTracker_CallbackNil(t *testing.T) {
	// NewBackoffTracker() should create a tracker without callback
	bt := NewBackoffTracker()

	level := bt.Increment("model1")
	if level != 1 {
		t.Errorf("expected level 1, got %d", level)
	}

	// Should not panic even though no callback is set
	bt.Reset("model1")
}

func TestCircuitBreaker_NilCallbackSafety(t *testing.T) {
	cb := NewCircuitBreaker(1, 1, 1, time.Second, nil)

	// Should not panic even though no callback is set
	cb.RecordFailureWithStatus("test:model", 429, "rate limit exceeded")

	if cb.IsModelLocked("model") {
		t.Error("model should be locked")
	}
}

func TestBackoffTracker_Callback_Concurrent(t *testing.T) {
	bt := NewBackoffTrackerWithCallback(func(model string, level int) {})

	var wg sync.WaitGroup
	numGoroutines := 50
	incrementsPerGoroutine := 100

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < incrementsPerGoroutine; j++ {
				model := "model" + string(rune('0'+id%10))
				bt.Increment(model)
			}
		}(i)
	}

	wg.Wait()

	// Check that each model reached cap
	for i := 0; i < 10; i++ {
		model := "model" + string(rune('0'+i))
		if level := bt.GetLevel(model); level != maxBackoffLevel {
			t.Errorf("model %s expected level %d, got %d", model, maxBackoffLevel, level)
		}
	}
}
