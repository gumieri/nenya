package resilience

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func BenchmarkCircuitBreaker_RecordFailureWithStatus_Serial(b *testing.B) {
	cb := NewCircuitBreaker(5, 1, 1, time.Minute, nil)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		cb.RecordFailureWithStatus("test:model", 429, "rate limit exceeded")
	}
}

func BenchmarkCircuitBreaker_RecordFailureWithStatus_Parallel(b *testing.B) {
	cb := NewCircuitBreaker(5, 1, 1, time.Minute, nil)
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			cb.RecordFailureWithStatus("test:model", 429, "rate limit exceeded")
		}
	})
}

func BenchmarkCircuitBreaker_RecordFailureWithStatus_MultipleModels(b *testing.B) {
	cb := NewCircuitBreaker(5, 1, 1, time.Minute, nil)
	models := []string{"model1", "model2", "model3", "model4", "model5"}
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		model := models[i%len(models)]
		cb.RecordFailureWithStatus("test:"+model, 429, "rate limit exceeded")
	}
}

func BenchmarkCircuitBreaker_RecordFailureWithStatus_MultipleModels_Parallel(b *testing.B) {
	cb := NewCircuitBreaker(5, 1, 1, time.Minute, nil)
	models := []string{"model1", "model2", "model3", "model4", "model5"}
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			model := models[len(models)%5]
			cb.RecordFailureWithStatus("test:"+model, 429, "rate limit exceeded")
		}
	})
}

func BenchmarkCircuitBreaker_IsModelLocked_Parallel(b *testing.B) {
	cb := NewCircuitBreaker(5, 1, 1, time.Minute, nil)
	cb.RecordFailureWithStatus("test:model", 429, "rate limit exceeded")
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = cb.IsModelLocked("test:model")
		}
	})
}

func BenchmarkBackoffTracker_Increment_Parallel(b *testing.B) {
	bt := NewBackoffTracker()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			bt.Increment("model1")
		}
	})
}

func BenchmarkBackoffTracker_GetLevel_Parallel(b *testing.B) {
	bt := NewBackoffTracker()
	bt.Increment("model1")
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = bt.GetLevel("model1")
		}
	})
}

func BenchmarkClassifyHTTPError(b *testing.B) {
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		classifyHTTPError(429, "rate limit exceeded", 0)
	}
}

func BenchmarkComputeExponentialBackoff(b *testing.B) {
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		computeExponentialBackoff(5, 500)
	}
}

// BenchmarkCircuitBreaker_MixedOperations benchmarks concurrent mixed operations.
func BenchmarkCircuitBreaker_MixedOperations(b *testing.B) {
	cb := NewCircuitBreaker(5, 1, 1, time.Minute, nil)
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		var i int
		for pb.Next() {
			i++
			key := "test:model"
			switch i % 3 {
			case 0:
				cb.RecordFailureWithStatus(key, 429, "rate limit exceeded")
			case 1:
				cb.RecordFailureWithStatus(key, 500, "server error")
			case 2:
				_ = cb.IsModelLocked(key)
			}
		}
	})
}

// BenchmarkCircuitBreaker_RecordFailureWithStatus_Auth benchmarks auth error classification.
func BenchmarkCircuitBreaker_RecordFailureWithStatus_Auth(b *testing.B) {
	cb := NewCircuitBreaker(5, 1, 1, time.Minute, nil)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		cb.RecordFailureWithStatus("test:model", 401, "unauthorized")
	}
}

// BenchmarkCircuitBreaker_RecordFailureWithStatus_Server benchmarks server error classification.
func BenchmarkCircuitBreaker_RecordFailureWithStatus_Server(b *testing.B) {
	cb := NewCircuitBreaker(5, 1, 1, time.Minute, nil)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		cb.RecordFailureWithStatus("test:model", 500, "internal server error")
	}
}

// BenchmarkCircuitBreaker_WithCallback measures overhead of backoff increment callbacks.
func BenchmarkCircuitBreaker_WithCallback(b *testing.B) {
	cb := NewCircuitBreaker(5, 1, 1, time.Minute, nil)
	var callbackCount atomic.Int64
	cb.SetBackoffIncrementCallback(func(key string, level int) {
		callbackCount.Add(1)
	})
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		cb.RecordFailureWithStatus("test:model", 429, "rate limit exceeded")
	}
}

// BenchmarkCircuitBreaker_HighConcurrency is a stress test for high-concurrency scenarios.
// Note: This is not a true benchmark as it uses fixed iteration counts rather than b.N.
// It simulates 100 goroutines each performing 1000 RecordFailureWithStatus operations
// to verify thread safety under extreme load.
func BenchmarkCircuitBreaker_HighConcurrency(b *testing.B) {
	cb := NewCircuitBreaker(5, 1, 1, time.Minute, nil)
	var wg sync.WaitGroup
	numGoroutines := 100
	opsPerGoroutine := 1000

	b.ResetTimer()

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				model := "model" + string(rune('0'+goroutineID%10))
				cb.RecordFailureWithStatus("agent:provider:"+model, 429, "rate limit exceeded")
			}
		}(g)
	}
	wg.Wait()
}