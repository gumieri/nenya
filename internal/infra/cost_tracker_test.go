package infra

import (
	"sync"
	"testing"
)

func TestCostTracker_RecordUsage(t *testing.T) {
	ct := NewCostTracker()
	ct.RecordUsage("model-a", 0.001234)
	ct.RecordUsage("model-a", 0.000566)
	ct.RecordUsage("model-b", 0.01)

	costA := ct.GetCostUSD("model-a")
	costB := ct.GetCostUSD("model-b")

	expectedA := 0.001234 + 0.000566
	if costA != expectedA {
		t.Errorf("model-a cost: got %.6f, want %.6f", costA, expectedA)
	}
	if costB != 0.01 {
		t.Errorf("model-b cost: got %.6f, want 0.01", costB)
	}
}

func TestCostTracker_RecordUsage_Rounding(t *testing.T) {
	ct := NewCostTracker()
	ct.RecordUsage("model-a", 0.0000001)

	cost := ct.GetCostMicroUSD("model-a")
	if cost != 1 {
		t.Errorf("micro-USD: got %d, want 1", cost)
	}
}

func TestCostTracker_RecordError(t *testing.T) {
	ct := NewCostTracker()
	ct.RecordError("model-a")
	ct.RecordError("model-a")
	ct.RecordError("model-b")

	errA := ct.GetErrorCount("model-a")
	errB := ct.GetErrorCount("model-b")

	if errA != 2 {
		t.Errorf("model-a errors: got %d, want 2", errA)
	}
	if errB != 1 {
		t.Errorf("model-b errors: got %d, want 1", errB)
	}
}

func TestCostTracker_GetCostMicroUSD(t *testing.T) {
	ct := NewCostTracker()
	ct.RecordUsage("model-a", 0.001234)

	expected := int64(1234)
	cost := ct.GetCostMicroUSD("model-a")
	if cost != expected {
		t.Errorf("micro-USD: got %d, want %d", cost, expected)
	}
}

func TestCostTracker_GetCostUSD(t *testing.T) {
	ct := NewCostTracker()
	ct.RecordUsage("model-a", 0.001234)

	cost := ct.GetCostUSD("model-a")
	if cost != 0.001234 {
		t.Errorf("USD: got %.6f, want 0.001234", cost)
	}
}

func TestCostTracker_GetAllCostsMicroUSD(t *testing.T) {
	ct := NewCostTracker()
	ct.RecordUsage("model-a", 0.001)
	ct.RecordUsage("model-b", 0.002)
	ct.RecordUsage("model-c", 0.003)

	costs := ct.GetAllCostsMicroUSD()
	if len(costs) != 3 {
		t.Errorf("got %d models, want 3", len(costs))
	}
	if costs["model-a"] != 1000 {
		t.Errorf("model-a: got %d, want 1000", costs["model-a"])
	}
	if costs["model-b"] != 2000 {
		t.Errorf("model-b: got %d, want 2000", costs["model-b"])
	}
	if costs["model-c"] != 3000 {
		t.Errorf("model-c: got %d, want 3000", costs["model-c"])
	}
}

func TestCostTracker_GetAllErrors(t *testing.T) {
	ct := NewCostTracker()
	ct.RecordError("model-a")
	ct.RecordError("model-b")

	errors := ct.GetAllErrors()
	if len(errors) != 2 {
		t.Errorf("got %d models, want 2", len(errors))
	}
	if errors["model-a"] != 1 {
		t.Errorf("model-a: got %d, want 1", errors["model-a"])
	}
	if errors["model-b"] != 1 {
		t.Errorf("model-b: got %d, want 1", errors["model-b"])
	}
}

func TestCostTracker_Snapshot(t *testing.T) {
	ct := NewCostTracker()
	ct.RecordUsage("model-a", 0.001)
	ct.RecordUsage("model-b", 0.002)
	ct.RecordError("model-a")

	snap := ct.Snapshot()

	if snap.TotalCostMicroUSD != 3000 {
		t.Errorf("total: got %d, want 3000", snap.TotalCostMicroUSD)
	}
	if len(snap.ModelCosts) != 2 {
		t.Errorf("model costs: got %d, want 2", len(snap.ModelCosts))
	}
	if len(snap.ModelErrors) != 1 {
		t.Errorf("model errors: got %d, want 1", len(snap.ModelErrors))
	}
}

func TestCostTracker_Concurrency(t *testing.T) {
	ct := NewCostTracker()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ct.RecordUsage("concurrent", 0.01)
			ct.RecordError("concurrent")
		}()
	}
	wg.Wait()

	cost := ct.GetCostUSD("concurrent")
	errors := ct.GetErrorCount("concurrent")

	if cost != 1.0 {
		t.Errorf("cost: got %.2f, want 1.00", cost)
	}
	if errors != 100 {
		t.Errorf("errors: got %d, want 100", errors)
	}
}

func TestCostTracker_ZeroCost(t *testing.T) {
	ct := NewCostTracker()
	cost := ct.GetCostUSD("nonexistent")
	if cost != 0 {
		t.Errorf("got %.6f, want 0", cost)
	}
}

func TestCostTracker_ZeroError(t *testing.T) {
	ct := NewCostTracker()
	errors := ct.GetErrorCount("nonexistent")
	if errors != 0 {
		t.Errorf("got %d, want 0", errors)
	}
}

func TestCostTracker_NegativeCost_Allowed(t *testing.T) {
	ct := NewCostTracker()
	ct.RecordUsage("model-a", -0.01)

	cost := ct.GetCostMicroUSD("model-a")
	if cost != -10000 {
		t.Errorf("negative costs are allowed (for refunds), got %d, want -10000", cost)
	}
}
