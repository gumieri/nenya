package infra

import (
	"sync"
	"testing"
)

func TestUsageTracker_RecordRequest(t *testing.T) {
	u := NewUsageTracker()
	u.RecordRequest("model-a", 100)
	u.RecordRequest("model-a", 50)
	u.RecordRequest("model-b", 200)

	snap := u.Snapshot()
	models := snap["models"].(map[string]interface{})
	ma := models["model-a"].(map[string]interface{})
	mb := models["model-b"].(map[string]interface{})

	if ma["requests"].(uint64) != 2 {
		t.Errorf("model-a requests: got %d, want 2", ma["requests"])
	}
	if ma["input_tokens"].(uint64) != 150 {
		t.Errorf("model-a input_tokens: got %d, want 150", ma["input_tokens"])
	}
	if mb["requests"].(uint64) != 1 {
		t.Errorf("model-b requests: got %d, want 1", mb["requests"])
	}
	if mb["input_tokens"].(uint64) != 200 {
		t.Errorf("model-b input_tokens: got %d, want 200", mb["input_tokens"])
	}
}

func TestUsageTracker_RecordOutput(t *testing.T) {
	u := NewUsageTracker()
	u.RecordOutput("model-a", 42)
	u.RecordOutput("model-a", 8)

	snap := u.Snapshot()
	models := snap["models"].(map[string]interface{})
	ma := models["model-a"].(map[string]interface{})

	if ma["output_tokens"].(uint64) != 50 {
		t.Errorf("output_tokens: got %d, want 50", ma["output_tokens"])
	}
}

func TestUsageTracker_RecordError(t *testing.T) {
	u := NewUsageTracker()
	u.RecordError("model-a")
	u.RecordError("model-a")
	u.RecordError("model-b")

	snap := u.Snapshot()
	models := snap["models"].(map[string]interface{})
	ma := models["model-a"].(map[string]interface{})
	mb := models["model-b"].(map[string]interface{})

	if ma["errors"].(uint64) != 2 {
		t.Errorf("model-a errors: got %d, want 2", ma["errors"])
	}
	if mb["errors"].(uint64) != 1 {
		t.Errorf("model-b errors: got %d, want 1", mb["errors"])
	}
}

func TestUsageTracker_Concurrency(t *testing.T) {
	u := NewUsageTracker()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u.RecordRequest("concurrent", 10)
			u.RecordOutput("concurrent", 5)
			u.RecordError("concurrent")
		}()
	}
	wg.Wait()

	snap := u.Snapshot()
	models := snap["models"].(map[string]interface{})
	m := models["concurrent"].(map[string]interface{})

	if m["requests"].(uint64) != 100 {
		t.Errorf("requests: got %d, want 100", m["requests"])
	}
	if m["input_tokens"].(uint64) != 1000 {
		t.Errorf("input_tokens: got %d, want 1000", m["input_tokens"])
	}
	if m["output_tokens"].(uint64) != 500 {
		t.Errorf("output_tokens: got %d, want 500", m["output_tokens"])
	}
	if m["errors"].(uint64) != 100 {
		t.Errorf("errors: got %d, want 100", m["errors"])
	}
}

func TestUsageTracker_Snapshot_Empty(t *testing.T) {
	u := NewUsageTracker()
	snap := u.Snapshot()

	if _, ok := snap["uptime_seconds"]; !ok {
		t.Error("expected uptime_seconds")
	}
	if _, ok := snap["models"]; !ok {
		t.Error("expected models")
	}
	models := snap["models"].(map[string]interface{})
	if len(models) != 0 {
		t.Errorf("expected 0 models, got %d", len(models))
	}
}
