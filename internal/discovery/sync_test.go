package discovery

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testCacheLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestNewPersistentProviderCache(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cache := NewPersistentProviderCache(tmpDir, 1*time.Hour, testCacheLogger())
	if cache.cacheDir != tmpDir {
		t.Errorf("expected cacheDir %q, got %q", tmpDir, cache.cacheDir)
	}
	if cache.cacheTTL != 1*time.Hour {
		t.Errorf("expected TTL 1h, got %v", cache.cacheTTL)
	}
}

func TestPersistentProviderCache_SaveAndLoad(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cache := NewPersistentProviderCache(tmpDir, 1*time.Hour, testCacheLogger())

	result := ProviderDiscoveryResult{
		ProviderRef: "test-provider",
		Models: []DiscoveredModel{
			{ID: "model-1"},
			{ID: "model-2"},
		},
		Timestamp: time.Now(),
	}

	cache.Save(result)
	cache.Load()

	loaded, ok := cache.Get("test-provider")
	if !ok {
		t.Fatal("expected to find cached result")
	}
	if len(loaded.Models) != 2 {
		t.Errorf("expected 2 models, got %d", len(loaded.Models))
	}
	if loaded.ProviderRef != "test-provider" {
		t.Errorf("expected provider 'test-provider', got %q", loaded.ProviderRef)
	}
}

func TestPersistentProviderCache_Load_Expired(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cache := NewPersistentProviderCache(tmpDir, 10*time.Millisecond, testCacheLogger())

	result := ProviderDiscoveryResult{
		ProviderRef: "test-provider",
		Models:      []DiscoveredModel{{ID: "m1"}},
		Timestamp:   time.Now(),
	}

	cache.Save(result)
	time.Sleep(20 * time.Millisecond)

	freshCache := NewPersistentProviderCache(tmpDir, 10*time.Millisecond, testCacheLogger())
	freshCache.Load()

	_, ok := freshCache.Get("test-provider")
	if ok {
		t.Error("expected expired entry to be removed")
	}

	if _, err := os.Stat(filepath.Join(tmpDir, "test-provider.json")); !os.IsNotExist(err) {
		t.Error("expected cache file to be deleted")
	}
}

func TestPersistentProviderCache_Load_NonJSONFiles(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := os.WriteFile(filepath.Join(tmpDir, "readme.txt"), []byte("not json"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, "subdir"), 0755); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}

	cache := NewPersistentProviderCache(tmpDir, 1*time.Hour, testCacheLogger())
	cache.Load()

	if len(cache.GetAll()) != 0 {
		t.Error("expected empty cache after loading non-JSON files")
	}
}

func TestPersistentProviderCache_Load_InvalidJSON(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cachePath := filepath.Join(tmpDir, "provider.json")
	if err := os.WriteFile(cachePath, []byte("{invalid json}"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	cache := NewPersistentProviderCache(tmpDir, 1*time.Hour, testCacheLogger())
	cache.Load()

	_, ok := cache.Get("provider")
	if ok {
		t.Error("expected false for invalid JSON")
	}
}

func TestPersistentProviderCache_Get(t *testing.T) {
	cache := NewPersistentProviderCache("", 	1*time.Hour, testCacheLogger())

	_, ok := cache.Get("nonexistent")
	if ok {
		t.Error("expected false for nonexistent key")
	}
}

func TestPersistentProviderCache_GetAll(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cache := NewPersistentProviderCache(tmpDir, 1*time.Hour, testCacheLogger())

	cache.Save(ProviderDiscoveryResult{ProviderRef: "p1"})
	cache.Save(ProviderDiscoveryResult{ProviderRef: "p2"})

	all := cache.GetAll()
	if len(all) != 2 {
		t.Errorf("expected 2 entries, got %d", len(all))
	}

	all["p1"] = ProviderDiscoveryResult{ProviderRef: "modified"}
	original, _ := cache.Get("p1")
	if original.ProviderRef == "modified" {
		t.Error("GetAll should return a copy")
	}
}

func TestPersistentProviderCache_Clear(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cache := NewPersistentProviderCache(tmpDir, 1*time.Hour, testCacheLogger())

	cache.Save(ProviderDiscoveryResult{ProviderRef: "p1"})
	cache.Save(ProviderDiscoveryResult{ProviderRef: "p2"})

	if err := cache.Clear(); err != nil {
		t.Fatalf("Clear failed: %v", err)
	}

	if len(cache.GetAll()) != 0 {
		t.Error("expected empty cache after clear")
	}

	entries, _ := os.ReadDir(tmpDir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			t.Errorf("expected no JSON files, found %s", e.Name())
		}
	}
}

func TestPersistentProviderCache_Clear_EmptyDir(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cache := NewPersistentProviderCache(tmpDir, 1*time.Hour, testCacheLogger())

	if err := cache.Clear(); err != nil {
		t.Errorf("expected no error for empty dir, got: %v", err)
	}
}

func TestPersistentProviderCache_Prune(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cache := NewPersistentProviderCache(tmpDir, 	100*time.Millisecond, testCacheLogger())

	oldResult := ProviderDiscoveryResult{
		ProviderRef: "old",
		Timestamp:   time.Now().Add(-200 * time.Millisecond),
	}
	newResult := ProviderDiscoveryResult{
		ProviderRef: "new",
		Timestamp:   time.Now(),
	}

	cache.Save(oldResult)
	cache.Save(newResult)

	cache.Prune()

	_, ok := cache.Get("old")
	if ok {
		t.Error("expected old entry to be pruned")
	}

	_, ok = cache.Get("new")
	if !ok {
		t.Error("expected new entry to remain")
	}
}

func TestPersistentProviderCache_JSONRoundTrip(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cache := NewPersistentProviderCache(tmpDir, 1*time.Hour, testCacheLogger())

	original := ProviderDiscoveryResult{
		ProviderRef: "test",
		Models: []DiscoveredModel{
			{ID: "m1", MaxContext: 128000},
			{ID: "m2", MaxContext: 64000},
		},
		Timestamp: time.Now().UTC().Truncate(time.Millisecond),
		Error:     "test error",
	}

	cache.Save(original)
	cache.Load()

	loaded, ok := cache.Get("test")
	if !ok {
		t.Fatal("failed to load cached result")
	}

	if loaded.ProviderRef != original.ProviderRef {
		t.Errorf("provider_ref: got %q, want %q", loaded.ProviderRef, original.ProviderRef)
	}
	if len(loaded.Models) != len(original.Models) {
		t.Errorf("models count: got %d, want %d", len(loaded.Models), len(original.Models))
	}
	if loaded.Timestamp.Unix() != original.Timestamp.Unix() {
		t.Errorf("timestamp: got %v, want %v", loaded.Timestamp, original.Timestamp)
	}
	if loaded.Error != original.Error {
		t.Errorf("error: got %q, want %q", loaded.Error, original.Error)
	}
}

func TestPersistentProviderCache_MarshalIndent(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cache := NewPersistentProviderCache(tmpDir, 1*time.Hour, testCacheLogger())

	cache.Save(ProviderDiscoveryResult{
		ProviderRef: "p1",
		Models:      []DiscoveredModel{{ID: "m1"}},
		Timestamp:   time.Now(),
	})

	data, err := os.ReadFile(filepath.Join(tmpDir, "p1.json"))
	if err != nil {
		t.Fatalf("failed to read cache file: %v", err)
	}

	if !json.Valid(data) {
		t.Error("expected valid JSON")
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if _, ok := m["provider_ref"]; !ok {
		t.Error("expected 'provider_ref' field in JSON")
	}
}

func TestPersistentProviderCache_Save_Overwrite(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cache := NewPersistentProviderCache(tmpDir, 1*time.Hour, testCacheLogger())

	cache.Save(ProviderDiscoveryResult{
		ProviderRef: "p1",
		Models:      []DiscoveredModel{{ID: "old-model"}},
		Timestamp:   time.Now(),
	})

	cache.Save(ProviderDiscoveryResult{
		ProviderRef: "p1",
		Models:      []DiscoveredModel{{ID: "new-model"}},
		Timestamp:   time.Now().Add(1 * time.Second),
	})

	cache.Load()

	loaded, _ := cache.Get("p1")
	if len(loaded.Models) != 1 || loaded.Models[0].ID != "new-model" {
		t.Error("expected new model after overwrite")
	}
}
