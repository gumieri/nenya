package resilience

import (
	"testing"
	"time"
)

func TestClassifyHTTPError(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		backoffLevel  int
		expectedClass ErrorClass
		shouldLock    bool
		minCooldown   time.Duration
		maxCooldown   time.Duration
	}{
		{"401 auth", 401, "unauthorized", 0, ErrorClassAuth, true, 2 * time.Minute, 2 * time.Minute},
		{"403 forbidden", 403, "forbidden", 0, ErrorClassAuth, true, 2 * time.Minute, 2 * time.Minute},
		{"429 rate limit", 429, "rate limit exceeded", 0, ErrorClassRate, true, 475 * time.Millisecond, 525 * time.Millisecond},
		{"429 with backoff", 429, "rate limit", 1, ErrorClassRate, true, 950 * time.Millisecond, 1050 * time.Millisecond},
		{"400 quota", 400, "quota exceeded", 0, ErrorClassQuota, true, 57 * time.Second, 63 * time.Second},
		{"400 insufficient", 400, "insufficient quota", 0, ErrorClassQuota, true, 57 * time.Second, 63 * time.Second},
		{"402 payment", 402, "insufficient quota", 0, ErrorClassQuota, true, 57 * time.Second, 63 * time.Second},
		{"502 capacity", 502, "capacity exceeded", 0, ErrorClassCapacity, true, 28500 * time.Millisecond, 31500 * time.Millisecond},
		{"503 overload", 503, "overloaded", 0, ErrorClassCapacity, true, 28500 * time.Millisecond, 31500 * time.Millisecond},
		{"504 gateway timeout", 504, "timeout", 0, ErrorClassServer, true, 1 * time.Second, 1 * time.Second},
		{"400 generic", 400, "bad request", 0, ErrorClassUnknown, false, 5 * time.Second, 5 * time.Second},
		{"404 not found", 404, "not found", 0, ErrorClassUnknown, true, 30 * time.Second, 30 * time.Second},
		{"500 server error", 500, "internal server error", 0, ErrorClassServer, true, 1 * time.Second, 1 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := classifyHTTPError(tt.status, tt.body, tt.backoffLevel)

			if decision.Class != tt.expectedClass {
				t.Errorf("expected class %s, got %s", tt.expectedClass, decision.Class)
			}

			if decision.ShouldLock != tt.shouldLock {
				t.Errorf("expected ShouldLock %v, got %v", tt.shouldLock, decision.ShouldLock)
			}

			if decision.Cooldown < tt.minCooldown || decision.Cooldown > tt.maxCooldown {
				t.Errorf("expected cooldown between %v and %v, got %v", tt.minCooldown, tt.maxCooldown, decision.Cooldown)
			}
		})
	}
}

func TestClassifyHTTPError_BackoffIncrement(t *testing.T) {
	decision := classifyHTTPError(429, "rate limit", 0)
	if !decision.IncrementBackoff {
		t.Error("rate limit should increment backoff")
	}

	decision = classifyHTTPError(400, "quota exceeded", 0)
	if !decision.IncrementBackoff {
		t.Error("quota should increment backoff")
	}

	decision = classifyHTTPError(503, "capacity", 0)
	if !decision.IncrementBackoff {
		t.Error("capacity should increment backoff")
	}

	decision = classifyHTTPError(401, "unauthorized", 0)
	if decision.IncrementBackoff {
		t.Error("auth should not increment backoff")
	}
}

func TestClassifyHTTPError_CaseInsensitive(t *testing.T) {
	decision := classifyHTTPError(429, "RATE LIMIT EXCEEDED", 0)
	if decision.Class != ErrorClassRate {
		t.Errorf("expected rate limit classification, got %s", decision.Class)
	}

	decision = classifyHTTPError(400, "Quota Exceeded", 0)
	if decision.Class != ErrorClassQuota {
		t.Errorf("expected quota classification, got %s", decision.Class)
	}

	decision = classifyHTTPError(503, "OVERLOAD", 0)
	if decision.Class != ErrorClassCapacity {
		t.Errorf("expected capacity classification, got %s", decision.Class)
	}
}

func TestClassifyHTTPError_SubstringMatch(t *testing.T) {
	body := `{
		"error": {
			"message": "Insufficient quota for this request",
			"type": "quota_exceeded"
		}
	}`

	decision := classifyHTTPError(400, body, 0)
	if decision.Class != ErrorClassQuota {
		t.Errorf("expected quota classification from JSON body, got %s", decision.Class)
	}

	body = `{
		"error": "capacity exceeded for model gpt-4"
	}`

	decision = classifyHTTPError(503, body, 0)
	if decision.Class != ErrorClassCapacity {
		t.Errorf("expected capacity classification from JSON body, got %s", decision.Class)
	}
}

func TestExponentialBackoff(t *testing.T) {
	tests := []struct {
		name        string
		level       int
		baseMs      int64
		minExpected time.Duration
		maxExpected time.Duration
	}{
		{"level 0", 0, 500, 475 * time.Millisecond, 525 * time.Millisecond},
		{"level 1", 1, 500, 950 * time.Millisecond, 1050 * time.Millisecond},
		{"level 2", 2, 500, 1900 * time.Millisecond, 2100 * time.Millisecond},
		{"level 5", 5, 500, 15200 * time.Millisecond, 16800 * time.Millisecond},
		{"level 10", 10, 500, 486400 * time.Millisecond, 537600 * time.Millisecond},
		{"quota level 0", 0, 60000, 57 * time.Second, 63 * time.Second},
		{"quota level 1", 1, 60000, 114 * time.Second, 126 * time.Second},
		{"capacity level 0", 0, 30000, 28500 * time.Millisecond, 31500 * time.Millisecond},
		{"capacity level 1", 1, 30000, 57 * time.Second, 63 * time.Second},
		{"negative level", -1, 500, 475 * time.Millisecond, 525 * time.Millisecond},
		{"max level", maxBackoffLevel, 500, time.Duration(int64((500<<uint64(maxBackoffLevel))*95)/100) * time.Millisecond, time.Duration(int64((500<<uint64(maxBackoffLevel))*105)/100) * time.Millisecond},
		{"above max", maxBackoffLevel + 1, 500, time.Duration(int64((500<<uint64(maxBackoffLevel))*95)/100) * time.Millisecond, time.Duration(int64((500<<uint64(maxBackoffLevel))*105)/100) * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := computeExponentialBackoff(tt.level, tt.baseMs)
			if result < tt.minExpected || result > tt.maxExpected {
				t.Errorf("computeExponentialBackoff(%d, %d) = %v, want %v-%v", tt.level, tt.baseMs, result, tt.minExpected, tt.maxExpected)
			}
		})
	}
}

func TestExponentialBackoff_LargeValues(t *testing.T) {
	result := computeExponentialBackoff(20, 60000)
	if result <= 0 {
		t.Error("expected positive duration for large level")
	}

	result = computeExponentialBackoff(100, 60000)
	if result <= 0 {
		t.Error("expected positive duration for very large level")
	}
}

func TestExponentialBackoffWithJitter(t *testing.T) {
	// Test that jitter is applied and within ±5% range
	samples := make([]time.Duration, 100)
	for i := 0; i < len(samples); i++ {
		samples[i] = computeExponentialBackoffWithJitter(3, 500)
	}

	base := computeExponentialBackoff(3, 500)
	minJitter := time.Duration(float64(base) * 0.95)
	maxJitter := time.Duration(float64(base) * 1.05)

	for _, sample := range samples {
		if sample < minJitter || sample > maxJitter {
			t.Errorf("jittered duration %v outside range [%v, %v]", sample, minJitter, maxJitter)
		}
	}

	// Test variance - samples should differ
	var hasVariance bool
	for i := 1; i < len(samples); i++ {
		if samples[i] != samples[0] {
			hasVariance = true
			break
		}
	}
	if !hasVariance {
		t.Error("expected variance in jittered samples, all were identical")
	}
}
