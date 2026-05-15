package resilience

import (
	"testing"
)

func TestBackoffTracker_GetLevel(t *testing.T) {
	bt := NewBackoffTracker()

	level := bt.GetLevel("model1")
	if level != 0 {
		t.Errorf("expected initial level 0, got %d", level)
	}

	bt.levels["model1"] = 3
	level = bt.GetLevel("model1")
	if level != 3 {
		t.Errorf("expected level 3, got %d", level)
	}
}

func TestBackoffTracker_Increment(t *testing.T) {
	bt := NewBackoffTracker()

	level := bt.Increment("model1")
	if level != 1 {
		t.Errorf("expected first increment to return 1, got %d", level)
	}

	if bt.levels["model1"] != 1 {
		t.Errorf("expected stored level 1, got %d", bt.levels["model1"])
	}

	bt.Increment("model1")
	bt.Increment("model1")
	level = bt.Increment("model1")
	if level != 4 {
		t.Errorf("expected fourth increment to return 4, got %d", level)
	}

	if bt.levels["model1"] != 4 {
		t.Errorf("expected stored level 4, got %d", bt.levels["model1"])
	}
}

func TestBackoffTracker_Increment_Cap(t *testing.T) {
	bt := NewBackoffTracker()

	bt.levels["model1"] = maxBackoffLevel
	level := bt.Increment("model1")
	if level != maxBackoffLevel {
		t.Errorf("expected increment to cap at %d, got %d", maxBackoffLevel, level)
	}

	if bt.levels["model1"] != maxBackoffLevel {
		t.Errorf("expected stored level to remain at %d, got %d", maxBackoffLevel, bt.levels["model1"])
	}
}

func TestBackoffTracker_Reset(t *testing.T) {
	bt := NewBackoffTracker()

	bt.levels["model1"] = 5
	bt.Reset("model1")

	if _, exists := bt.levels["model1"]; exists {
		t.Error("expected model1 to be deleted after reset")
	}

	bt.Reset("nonexistent")
}

func TestBackoffTracker_NilSafe(t *testing.T) {
	var bt *BackoffTracker

	if bt.GetLevel("model") != 0 {
		t.Error("nil tracker should return 0")
	}

	if bt.Increment("model") != 0 {
		t.Error("nil tracker should return 0 on increment")
	}

	bt.Reset("model")
}

func TestBackoffTracker_ConcurrentSafety(t *testing.T) {
	bt := NewBackoffTracker()

	done := make(chan bool)
	for i := 0; i < 100; i++ {
		go func() {
			bt.Increment("concurrent")
			done <- true
		}()
	}

	for i := 0; i < 100; i++ {
		<-done
	}

	bt.Reset("concurrent")
}
