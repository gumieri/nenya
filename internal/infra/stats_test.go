package infra

import (
	"encoding/json"
	"math"
	"sync"
	"testing"
	"time"
)

// snapshotModel returns the model entry from a Snapshot result, failing the test
// with a clear message rather than panicking on a shape change.
func snapshotModel(t *testing.T, snap map[string]interface{}, model string) map[string]interface{} {
	t.Helper()
	models, ok := snap["models"].(map[string]interface{})
	if !ok {
		t.Fatalf("models missing or not a map: %T", snap["models"])
	}
	entry, ok := models[model].(map[string]interface{})
	if !ok {
		t.Fatalf("%s entry missing or not a map: %T", model, models[model])
	}
	return entry
}

// uintField reads a uint64 counter from a snapshot model entry, failing the test
// with a clear message rather than panicking on a shape change.
func uintField(t *testing.T, entry map[string]interface{}, key string) uint64 {
	t.Helper()
	v, ok := entry[key].(uint64)
	if !ok {
		t.Fatalf("%s missing or not uint64: %T", key, entry[key])
	}
	return v
}

func TestTokenSnapshot_JSON(t *testing.T) {
	snap := TokenSnapshot{
		InputTokens:  100,
		OutputTokens: 50,
		TotalTokens:  150,
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var decoded TokenSnapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.InputTokens != 100 {
		t.Errorf("expected InputTokens=100, got %d", decoded.InputTokens)
	}
	if decoded.OutputTokens != 50 {
		t.Errorf("expected OutputTokens=50, got %d", decoded.OutputTokens)
	}
	if decoded.TotalTokens != 150 {
		t.Errorf("expected TotalTokens=150, got %d", decoded.TotalTokens)
	}
}

func TestTokenSnapshot_ZeroValues(t *testing.T) {
	snap := TokenSnapshot{}
	if snap.InputTokens != 0 || snap.OutputTokens != 0 || snap.TotalTokens != 0 {
		t.Error("expected all zero values")
	}
}

func TestUsageTracker_RecordRequest(t *testing.T) {
	u := NewUsageTracker()
	u.RecordRequest("model-a", 100)
	u.RecordRequest("model-a", 50)
	u.RecordRequest("model-b", 200)

	snap := u.Snapshot()
	ma := snapshotModel(t, snap, "model-a")
	mb := snapshotModel(t, snap, "model-b")

	if got := uintField(t, ma, "requests"); got != 2 {
		t.Errorf("model-a requests: got %d, want 2", got)
	}
	if got := uintField(t, ma, "input_tokens"); got != 150 {
		t.Errorf("model-a input_tokens: got %d, want 150", got)
	}
	if got := uintField(t, mb, "requests"); got != 1 {
		t.Errorf("model-b requests: got %d, want 1", got)
	}
	if got := uintField(t, mb, "input_tokens"); got != 200 {
		t.Errorf("model-b input_tokens: got %d, want 200", got)
	}
}

func TestUsageTracker_RecordOutput(t *testing.T) {
	u := NewUsageTracker()
	u.RecordOutput("model-a", 42)
	u.RecordOutput("model-a", 8)

	snap := u.Snapshot()
	ma := snapshotModel(t, snap, "model-a")

	if got := uintField(t, ma, "output_tokens"); got != 50 {
		t.Errorf("output_tokens: got %d, want 50", got)
	}
}

func TestUsageTracker_RecordError(t *testing.T) {
	u := NewUsageTracker()
	u.RecordError("model-a")
	u.RecordError("model-a")
	u.RecordError("model-b")

	snap := u.Snapshot()
	ma := snapshotModel(t, snap, "model-a")
	mb := snapshotModel(t, snap, "model-b")

	if got := uintField(t, ma, "errors"); got != 2 {
		t.Errorf("model-a errors: got %d, want 2", got)
	}
	if got := uintField(t, mb, "errors"); got != 1 {
		t.Errorf("model-b errors: got %d, want 1", got)
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
	m := snapshotModel(t, snap, "concurrent")

	if got := uintField(t, m, "requests"); got != 100 {
		t.Errorf("requests: got %d, want 100", got)
	}
	if got := uintField(t, m, "input_tokens"); got != 1000 {
		t.Errorf("input_tokens: got %d, want 1000", got)
	}
	if got := uintField(t, m, "output_tokens"); got != 500 {
		t.Errorf("output_tokens: got %d, want 500", got)
	}
	if got := uintField(t, m, "errors"); got != 100 {
		t.Errorf("errors: got %d, want 100", got)
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
	models, ok := snap["models"].(map[string]interface{})
	if !ok {
		t.Fatalf("models missing or not a map: %T", snap["models"])
	}
	if len(models) != 0 {
		t.Errorf("expected 0 models, got %d", len(models))
	}
}

// TestUsageTracker_SnapshotModelKeySet pins the per-model key set owned by
// Snapshot so a future direct marshal of modelStats cannot silently change the
// /statsz schema.
func TestUsageTracker_SnapshotModelKeySet(t *testing.T) {
	u := NewUsageTracker()
	u.RecordRequest("model-a", 1)
	u.RecordCacheHit("model-a", 1)

	entry := snapshotModel(t, u.Snapshot(), "model-a")
	want := []string{
		"requests", "input_tokens", "output_tokens", "reasoning_tokens",
		"cache_hit_tokens", "cache_miss_tokens", "cache_creation_tokens",
		"cache_hit_ratio", "errors",
	}
	for _, k := range want {
		if _, ok := entry[k]; !ok {
			t.Errorf("snapshot entry missing key %q", k)
		}
	}
	if len(entry) != len(want) {
		t.Errorf("snapshot entry has %d keys, want %d: %v", len(entry), len(want), entry)
	}
}

func TestUsageTracker_CacheCreationTokens(t *testing.T) {
	u := NewUsageTracker()
	u.RecordCacheCreation("claude-3", 1234)
	u.RecordCacheCreation("claude-3", 5678)
	u.RecordCacheCreation("gpt-4", 1000)

	snap := u.Snapshot()
	c3 := snapshotModel(t, snap, "claude-3")
	g4 := snapshotModel(t, snap, "gpt-4")

	if got := uintField(t, c3, "cache_creation_tokens"); got != 6912 {
		t.Errorf("claude-3 cache_creation_tokens: got %d, want 6912", got)
	}
	if got := uintField(t, g4, "cache_creation_tokens"); got != 1000 {
		t.Errorf("gpt-4 cache_creation_tokens: got %d, want 1000", got)
	}
}

func TestUsageTracker_CacheHitTokens(t *testing.T) {
	u := NewUsageTracker()
	u.RecordCacheHit("claude-3", 5000)
	u.RecordCacheHit("claude-3", 3000)
	u.RecordCacheHit("gpt-4", 2000)

	snap := u.Snapshot()
	c3 := snapshotModel(t, snap, "claude-3")
	g4 := snapshotModel(t, snap, "gpt-4")

	if got := uintField(t, c3, "cache_hit_tokens"); got != 8000 {
		t.Errorf("claude-3 cache_hit_tokens: got %d, want 8000", got)
	}
	if got := uintField(t, g4, "cache_hit_tokens"); got != 2000 {
		t.Errorf("gpt-4 cache_hit_tokens: got %d, want 2000", got)
	}
}

func TestUsageTracker_CacheMissTokens(t *testing.T) {
	u := NewUsageTracker()
	u.RecordCacheMiss("claude-3", 10000)
	u.RecordCacheMiss("claude-3", 5000)
	u.RecordCacheMiss("gpt-4", 7000)

	snap := u.Snapshot()
	c3 := snapshotModel(t, snap, "claude-3")
	g4 := snapshotModel(t, snap, "gpt-4")

	if got := uintField(t, c3, "cache_miss_tokens"); got != 15000 {
		t.Errorf("claude-3 cache_miss_tokens: got %d, want 15000", got)
	}
	if got := uintField(t, g4, "cache_miss_tokens"); got != 7000 {
		t.Errorf("gpt-4 cache_miss_tokens: got %d, want 7000", got)
	}
}

// TestUsageTracker_CacheHitRatio covers the /statsz cache_hit_ratio derivation
// added for the cache-economics baseline: hit / (hit + miss + creation), 0 for
// a model with no prompt-cache traffic.
func TestUsageTracker_CacheHitRatio(t *testing.T) {
	const eps = 1e-9

	t.Run("cacheHitRatio derivation", func(t *testing.T) {
		cases := []struct {
			name                string
			hit, miss, creation uint64
			want                float64
		}{
			{name: "no traffic", hit: 0, miss: 0, creation: 0, want: 0},
			{name: "hit only", hit: 1000, miss: 0, creation: 0, want: 1},
			{name: "miss only", hit: 0, miss: 1000, creation: 0, want: 0},
			{name: "creation only", hit: 0, miss: 0, creation: 1000, want: 0},
			{name: "mixed", hit: 3000, miss: 9000, creation: 0, want: 0.25},
			{name: "creation counts as non-hit", hit: 1000, miss: 1000, creation: 2000, want: 0.25},
			{name: "even split", hit: 500, miss: 500, creation: 0, want: 0.5},
			{name: "max values stay finite", hit: math.MaxUint64, miss: math.MaxUint64, creation: 0, want: 0.5},
			{name: "2^53 boundary rounds to 0.5", hit: 1 << 53, miss: 1 << 53, creation: 0, want: 0.5},
			{name: "2^53+1 stays bounded", hit: 1<<53 + 1, miss: 1 << 53, creation: 0, want: 0.5},
			{name: "max single value", hit: 0, miss: math.MaxUint64, creation: 0, want: 0},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := cacheHitRatio(tc.hit, tc.miss, tc.creation)
				if math.IsNaN(got) || math.IsInf(got, 0) {
					t.Fatalf("cacheHitRatio(%d, %d, %d) = %v, want finite", tc.hit, tc.miss, tc.creation, got)
				}
				if got < 0 || got > 1 {
					t.Errorf("cacheHitRatio(%d, %d, %d) = %v, want within [0,1]", tc.hit, tc.miss, tc.creation, got)
				}
				if math.Abs(got-tc.want) > eps {
					t.Errorf("cacheHitRatio(%d, %d, %d) = %v, want %v", tc.hit, tc.miss, tc.creation, got, tc.want)
				}
			})
		}
	})

	t.Run("snapshot exposes ratio", func(t *testing.T) {
		u := NewUsageTracker()
		u.RecordCacheHit("deepseek-flash", 3000)
		u.RecordCacheMiss("deepseek-flash", 9000)
		u.RecordRequest("deepseek-flash", 12000)

		snap := u.Snapshot()
		models, ok := snap["models"].(map[string]interface{})
		if !ok {
			t.Fatalf("models missing or not a map: %T", snap["models"])
		}
		entry, ok := models["deepseek-flash"].(map[string]interface{})
		if !ok {
			t.Fatalf("deepseek-flash entry missing or not a map: %T", models["deepseek-flash"])
		}

		got, ok := entry["cache_hit_ratio"].(float64)
		if !ok {
			t.Fatalf("cache_hit_ratio missing or not float64: %T (%v)", entry["cache_hit_ratio"], entry["cache_hit_ratio"])
		}
		if math.Abs(got-0.25) > eps {
			t.Errorf("cache_hit_ratio = %v, want 0.25", got)
		}

		// The exposed ratio must be self-consistent with the exposed token
		// fields of the same snapshot entry.
		hit, ok := entry["cache_hit_tokens"].(uint64)
		if !ok {
			t.Fatalf("cache_hit_tokens missing or not uint64: %T", entry["cache_hit_tokens"])
		}
		miss, ok := entry["cache_miss_tokens"].(uint64)
		if !ok {
			t.Fatalf("cache_miss_tokens missing or not uint64: %T", entry["cache_miss_tokens"])
		}
		creation, ok := entry["cache_creation_tokens"].(uint64)
		if !ok {
			t.Fatalf("cache_creation_tokens missing or not uint64: %T", entry["cache_creation_tokens"])
		}
		want := float64(hit) / (float64(hit) + float64(miss) + float64(creation))
		if math.Abs(got-want) > eps {
			t.Errorf("cache_hit_ratio = %v, not self-consistent with token fields (%v)", got, want)
		}

		// /statsz serializes the snapshot as JSON; a non-finite value would
		// fail to marshal or produce an invalid token.
		encoded, err := json.Marshal(snap)
		if err != nil {
			t.Fatalf("json.Marshal(snapshot) error: %v", err)
		}
		var roundTripped map[string]interface{}
		if err := json.Unmarshal(encoded, &roundTripped); err != nil {
			t.Fatalf("json.Unmarshal(snapshot) error: %v", err)
		}
		modelsRT, ok := roundTripped["models"].(map[string]interface{})
		if !ok {
			t.Fatalf("round-tripped models missing or not a map: %T", roundTripped["models"])
		}
		entryRT, ok := modelsRT["deepseek-flash"].(map[string]interface{})
		if !ok {
			t.Fatalf("round-tripped deepseek-flash entry missing or not a map: %T", modelsRT["deepseek-flash"])
		}
		ratioRT, ok := entryRT["cache_hit_ratio"].(float64)
		if !ok {
			t.Fatalf("round-tripped cache_hit_ratio missing or not float64: %T (%v)", entryRT["cache_hit_ratio"], entryRT["cache_hit_ratio"])
		}
		if math.Abs(ratioRT-0.25) > eps {
			t.Errorf("round-tripped cache_hit_ratio = %v, want 0.25", ratioRT)
		}
	})

	t.Run("model without cache traffic reports zero", func(t *testing.T) {
		u := NewUsageTracker()
		u.RecordRequest("model-no-cache", 100)

		entry := snapshotModel(t, u.Snapshot(), "model-no-cache")
		got, ok := entry["cache_hit_ratio"].(float64)
		if !ok {
			t.Fatalf("cache_hit_ratio missing or not float64: %T (%v)", entry["cache_hit_ratio"], entry["cache_hit_ratio"])
		}
		if math.Abs(got) > eps {
			t.Errorf("cache_hit_ratio = %v, want 0", got)
		}
	})
}

// TestUsageTracker_NegativeTokenGuards verifies that negative token counts do
// not wrap the unsigned counters or create wrapped entries, and that a negative
// RecordRequest still counts the request it represents.
func TestUsageTracker_NegativeTokenGuards(t *testing.T) {
	t.Run("negative count before entry creation does not create an entry", func(t *testing.T) {
		u := NewUsageTracker()
		u.RecordOutput("neg-only", -1)
		snap := u.Snapshot()
		models, ok := snap["models"].(map[string]interface{})
		if !ok {
			t.Fatalf("models missing or not a map: %T", snap["models"])
		}
		if _, exists := models["neg-only"]; exists {
			t.Error("a negative count created a stats entry")
		}
	})

	t.Run("every negative token recorder skips entry creation", func(t *testing.T) {
		recorders := map[string]func(u *UsageTracker, model string){
			"RecordOutput":        func(u *UsageTracker, m string) { u.RecordOutput(m, -1) },
			"RecordCacheHit":      func(u *UsageTracker, m string) { u.RecordCacheHit(m, -1) },
			"RecordCacheMiss":     func(u *UsageTracker, m string) { u.RecordCacheMiss(m, -1) },
			"RecordCacheCreation": func(u *UsageTracker, m string) { u.RecordCacheCreation(m, -1) },
			"RecordReasoning":     func(u *UsageTracker, m string) { u.RecordReasoning(m, -1) },
		}
		for name, rec := range recorders {
			t.Run(name, func(t *testing.T) {
				u := NewUsageTracker()
				rec(u, "neg-"+name)
				models, _ := u.Snapshot()["models"].(map[string]interface{})
				if _, exists := models["neg-"+name]; exists {
					t.Errorf("%s skipped the negative guard and created an entry", name)
				}
			})
		}
	})

	t.Run("negative counts never wrap, request still counted", func(t *testing.T) {
		u := NewUsageTracker()
		u.RecordRequest("m", -50)
		u.RecordOutput("m", -1)
		u.RecordCacheHit("m", -1)
		u.RecordCacheMiss("m", -1)
		u.RecordCacheCreation("m", -1)
		u.RecordReasoning("m", -1)

		snap := u.Snapshot()
		models, ok := snap["models"].(map[string]interface{})
		if !ok {
			t.Fatalf("models missing or not a map: %T", snap["models"])
		}
		entry, ok := models["m"].(map[string]interface{})
		if !ok {
			t.Fatalf("m entry missing or not a map: %T", models["m"])
		}

		if got := uintField(t, entry, "requests"); got != 1 {
			t.Errorf("requests = %d, want 1 (negative input tokens must not drop the request)", got)
		}
		for _, key := range []string{
			"input_tokens", "output_tokens", "reasoning_tokens",
			"cache_hit_tokens", "cache_miss_tokens", "cache_creation_tokens",
		} {
			if got := uintField(t, entry, key); got != 0 {
				t.Errorf("%s = %d, want 0 after negative records", key, got)
			}
		}

		// Positive counts are still recorded after the negative ones.
		u.RecordOutput("m", 7)
		u.RecordCacheHit("m", 5)
		u.RecordCacheMiss("m", 5)

		snap = u.Snapshot()
		models, ok = snap["models"].(map[string]interface{})
		if !ok {
			t.Fatalf("models missing or not a map on re-snapshot: %T", snap["models"])
		}
		entry, ok = models["m"].(map[string]interface{})
		if !ok {
			t.Fatalf("m entry missing or not a map on re-snapshot: %T", models["m"])
		}
		if got := uintField(t, entry, "output_tokens"); got != 7 {
			t.Errorf("output_tokens = %d, want 7", got)
		}
		if got := uintField(t, entry, "cache_hit_tokens"); got != 5 {
			t.Errorf("cache_hit_tokens = %d, want 5", got)
		}
		if got := uintField(t, entry, "cache_miss_tokens"); got != 5 {
			t.Errorf("cache_miss_tokens = %d, want 5", got)
		}
	})
}

// TestUsageTracker_GetOrCreateIdentity verifies getOrCreate returns a stable
// pointer for a given model and a distinct pointer across models.
func TestUsageTracker_GetOrCreateIdentity(t *testing.T) {
	u := NewUsageTracker()
	first := u.getOrCreate("model-a")
	if again := u.getOrCreate("model-a"); again != first {
		t.Error("getOrCreate returned a different pointer for the same model")
	}
	if other := u.getOrCreate("model-b"); other == first {
		t.Error("getOrCreate returned the same pointer for different models")
	}

	t.Run("concurrent creation converges on one pointer", func(t *testing.T) {
		cu := NewUsageTracker()
		const n = 32
		ptrs := make([]*modelStats, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				ptrs[i] = cu.getOrCreate("same")
			}(i)
		}
		close(start)
		wg.Wait()
		for i := 1; i < n; i++ {
			if ptrs[i] != ptrs[0] {
				t.Fatalf("goroutine %d got a different pointer than goroutine 0", i)
			}
		}
	})

	t.Run("snapshot readers never observe a partial entry", func(t *testing.T) {
		ru := NewUsageTracker()
		const writers = 16
		keys := []string{"requests", "input_tokens", "cache_hit_tokens", "cache_hit_ratio", "errors"}
		stop := make(chan struct{})
		var readers sync.WaitGroup
		// Concurrent readers: a first-time create must never be visible as a
		// partially-initialized entry (publish-after-construct). Each reader
		// asserts the full key set the moment the entry appears, so a
		// publish-then-populate regression fails here.
		for i := 0; i < 4; i++ {
			readers.Add(1)
			go func() {
				defer readers.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					models, _ := ru.Snapshot()["models"].(map[string]interface{})
					entry, ok := models["shared"].(map[string]interface{})
					if !ok {
						continue
					}
					for _, key := range keys {
						if _, ok := entry[key]; !ok {
							t.Errorf("snapshot exposed a partial entry: missing %q", key)
							return
						}
					}
				}
			}()
		}
		// Fan out the first-time creates so the readers overlap construction.
		var writersWG sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < writers; i++ {
			writersWG.Add(1)
			go func() {
				defer writersWG.Done()
				<-start
				ru.getOrCreate("shared")
			}()
		}
		close(start)
		writersWG.Wait()
		// Give the readers a window to observe the published entry, then stop.
		time.Sleep(5 * time.Millisecond)
		close(stop)
		readers.Wait()

		entry := snapshotModel(t, ru.Snapshot(), "shared")
		for _, key := range keys {
			if _, ok := entry[key]; !ok {
				t.Errorf("snapshot entry missing %q", key)
			}
		}
	})
}
