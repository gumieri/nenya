package infra

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFingerprintPayload_Deterministic(t *testing.T) {
	payload := map[string]interface{}{
		"model":       "gpt-4",
		"messages":    []interface{}{map[string]interface{}{"role": "user", "content": "hello"}},
		"temperature": 0.7,
	}
	h1 := FingerprintPayload(payload)
	h2 := FingerprintPayload(payload)
	if h1 == "" {
		t.Fatal("fingerprint returned empty string")
	}
	if h1 != h2 {
		t.Fatalf("same payload produced different fingerprints: %q != %q", h1, h2)
	}
}

func TestFingerprintPayload_KeyOrdering(t *testing.T) {
	json1 := `{"messages":[{"role":"user","content":"hi"}],"model":"gpt-4","temperature":0.5}`
	json2 := `{"temperature":0.5,"model":"gpt-4","messages":[{"content":"hi","role":"user"}]}`

	var p1, p2 map[string]interface{}
	if err := json.Unmarshal([]byte(json1), &p1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(json2), &p2); err != nil {
		t.Fatal(err)
	}

	h1 := FingerprintPayload(p1)
	h2 := FingerprintPayload(p2)
	if h1 != h2 {
		t.Fatalf("different key orderings produced different fingerprints: %q != %q", h1, h2)
	}
}

func TestFingerprintPayload_ExcludedFields(t *testing.T) {
	base := map[string]interface{}{
		"model":       "gpt-4",
		"messages":    []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"temperature": 0.5,
		"top_p":       0.9,
	}
	h1 := FingerprintPayload(base)

	extra := map[string]interface{}{
		"model":       "gpt-4",
		"messages":    []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"temperature": 0.5,
		"top_p":       0.9,
		"extra":       "should be ignored",
	}
	h2 := FingerprintPayload(extra)
	if h1 != h2 {
		t.Fatalf("extra field changed fingerprint: %q != %q", h1, h2)
	}
}

func TestFingerprintPayload_EmptyPayload(t *testing.T) {
	h := FingerprintPayload(map[string]interface{}{})
	if h == "" {
		t.Fatal("empty payload should still produce a fingerprint")
	}
}

func TestResponseCache_CacheMetrics(t *testing.T) {
	metrics := NewMetrics()
	cache := NewResponseCache(10, 1<<20, 1*time.Hour, 1*time.Hour, metrics, false, 0.9, nil)
	defer cache.Stop()

	cache.Store("key1", []byte("response1"), nil)

	data, ok, _ := cache.Lookup("key1", "", nil)
	if !ok || string(data) != "response1" {
		t.Fatal("cache hit should return value")
	}

	_, ok, _ = cache.Lookup("nonexistent", "", nil)
	if ok {
		t.Fatal("nonexistent key should not be found")
	}

	buf := &strings.Builder{}
	metrics.WritePrometheus(buf)
	output := buf.String()

	if !strings.Contains(output, `nenya_cache_hit_total{`) || !strings.Contains(output, `type="exact"`) {
		t.Error("expected cache hit metric, got:", output)
	}
	if !strings.Contains(output, `nenya_cache_miss_total{`) || !strings.Contains(output, `type="exact"`) {
		t.Error("expected cache miss metric, got:", output)
	}
}

func TestResponseCache_StoreAndLookup(t *testing.T) {
	cache := NewResponseCache(10, 1<<20, 1*time.Hour, 1*time.Hour, nil, false, 0.9, nil)
	defer cache.Stop()

	cache.Store("key1", []byte("response1"), nil)
	cache.Store("key2", []byte("response2"), nil)

	data, ok, _ := cache.Lookup("key1", "", nil)
	if !ok || string(data) != "response1" {
		t.Fatalf("expected response1, got %q, ok=%v", string(data), ok)
	}

	data, ok, _ = cache.Lookup("key2", "", nil)
	if !ok || string(data) != "response2" {
		t.Fatalf("expected response2, got %q, ok=%v", string(data), ok)
	}

	_, ok, _ = cache.Lookup("nonexistent", "", nil)
	if ok {
		t.Fatal("nonexistent key should not be found")
	}
}

func TestResponseCache_TTLExpiration(t *testing.T) {
	cache := NewResponseCache(10, 1<<20, 100*time.Millisecond, 50*time.Millisecond, nil, false, 0.9, nil)
	defer cache.Stop()

	cache.Store("key1", []byte("response1"), nil)

	data, ok, _ := cache.Lookup("key1", "", nil)
	if !ok || string(data) != "response1" {
		t.Fatal("should find entry immediately")
	}

	time.Sleep(150 * time.Millisecond)

	_, ok, _ = cache.Lookup("key1", "", nil)
	if ok {
		t.Fatal("expired entry should not be found")
	}

	if cache.Len() != 0 {
		t.Fatalf("expected 0 entries after expiration, got %d", cache.Len())
	}
}

func TestResponseCache_LRU_Eviction(t *testing.T) {
	cache := NewResponseCache(3, 1<<20, 1*time.Hour, 1*time.Hour, nil, false, 0.9, nil)
	defer cache.Stop()

	cache.Store("key1", []byte("a"), nil)
	cache.Store("key2", []byte("b"), nil)
	cache.Store("key3", []byte("c"), nil)

	if cache.Len() != 3 {
		t.Fatalf("expected 3 entries, got %d", cache.Len())
	}

	cache.Store("key4", []byte("d"), nil)

	if cache.Len() != 3 {
		t.Fatalf("expected 3 entries after eviction, got %d", cache.Len())
	}

	_, ok, _ := cache.Lookup("key1", "", nil)
	if ok {
		t.Fatal("oldest entry (key1) should have been evicted")
	}

	data, ok, _ := cache.Lookup("key2", "", nil)
	if !ok || string(data) != "b" {
		t.Fatal("key2 should still exist")
	}

	data, ok, _ = cache.Lookup("key4", "", nil)
	if !ok || string(data) != "d" {
		t.Fatal("key4 should exist")
	}
}

func TestResponseCache_MaxEntryBytes(t *testing.T) {
	cache := NewResponseCache(10, 100, 1*time.Hour, 1*time.Hour, nil, false, 0.9, nil)
	defer cache.Stop()

	cache.Store("small", []byte("hello"), nil)
	data, ok, _ := cache.Lookup("small", "", nil)
	if !ok || string(data) != "hello" {
		t.Fatal("small entry should be stored")
	}

	largeData := strings.Repeat("x", 200)
	cache.Store("large", []byte(largeData), nil)

	_, ok, _ = cache.Lookup("large", "", nil)
	if ok {
		t.Fatal("oversized entry should not be stored")
	}

	if cache.Len() != 1 {
		t.Fatalf("expected 1 entry, got %d", cache.Len())
	}
}

func TestResponseCache_StoreUpdate(t *testing.T) {
	cache := NewResponseCache(10, 1<<20, 1*time.Hour, 1*time.Hour, nil, false, 0.9, nil)
	defer cache.Stop()

	cache.Store("key1", []byte("v1"), nil)
	cache.Store("key1", []byte("v2"), nil)

	if cache.Len() != 1 {
		t.Fatalf("expected 1 entry after update, got %d", cache.Len())
	}

	data, ok, _ := cache.Lookup("key1", "", nil)
	if !ok || string(data) != "v2" {
		t.Fatalf("expected v2, got %q", data)
	}
}

func TestResponseCache_EmptyKeyAndData(t *testing.T) {
	cache := NewResponseCache(10, 1<<20, 1*time.Hour, 1*time.Hour, nil, false, 0.9, nil)
	defer cache.Stop()

	cache.Store("", []byte("data"), nil)
	if cache.Len() != 0 {
		t.Fatal("empty key should not be stored")
	}

	cache.Store("key", nil, nil)
	if cache.Len() != 0 {
		t.Fatal("nil data should not be stored")
	}

	cache.Store("key", []byte{}, nil)
	if cache.Len() != 0 {
		t.Fatal("empty data should not be stored")
	}
}

func TestResponseCache_BackgroundEvictor(t *testing.T) {
	cache := NewResponseCache(100, 1<<20, 200*time.Millisecond, 100*time.Millisecond, nil, false, 0.9, nil)
	defer cache.Stop()

	for i := 0; i < 50; i++ {
		cache.Store(fmt.Sprintf("key%d", i), []byte(fmt.Sprintf("val%d", i)), nil)
	}

	if cache.Len() != 50 {
		t.Fatalf("expected 50 entries, got %d", cache.Len())
	}

	time.Sleep(350 * time.Millisecond)

	if cache.Len() != 0 {
		t.Fatalf("expected 0 entries after background eviction, got %d", cache.Len())
	}
}

func TestResponseCache_Stop(t *testing.T) {
	cache := NewResponseCache(10, 1<<20, 1*time.Hour, 50*time.Millisecond, nil, false, 0.9, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(200 * time.Millisecond)
		cache.Stop()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop should complete without hanging")
	}
}

func TestResponseCache_ConcurrentAccess(t *testing.T) {
	cache := NewResponseCache(100, 1<<20, 10*time.Minute, 1*time.Hour, nil, false, 0.9, nil)
	defer cache.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("key%d", idx)
			cache.Store(key, []byte(fmt.Sprintf("val%d", idx)), nil)
			time.Sleep(time.Microsecond * time.Duration(idx%5))
			cache.Lookup(key, "", nil)
			cache.Lookup("nonexistent", "", nil)
		}(i)
	}
	wg.Wait()

	expected := 100
	if cache.Len() != expected {
		t.Fatalf("expected %d entries after concurrent access, got %d", expected, cache.Len())
	}
}

func TestResponseCache_BackgroundEvictorRemovesExpiredFirst(t *testing.T) {
	cache := NewResponseCache(5, 1<<20, 50*time.Millisecond, 20*time.Millisecond, nil, false, 0.9, nil)
	defer cache.Stop()

	cache.Store("expires-soon", []byte("a"), nil)
	cache.Store("expires-soon-2", []byte("b"), nil)

	time.Sleep(70 * time.Millisecond)

	for i := 0; i < 3; i++ {
		cache.Store(fmt.Sprintf("fresh%d", i), []byte(fmt.Sprintf("f%d", i)), nil)
	}

	time.Sleep(40 * time.Millisecond)

	_, ok, _ := cache.Lookup("expires-soon", "", nil)
	if ok {
		t.Fatal("expired entries should be cleaned up by background evictor")
	}
	_, ok, _ = cache.Lookup("expires-soon-2", "", nil)
	if ok {
		t.Fatal("expired entries should be cleaned up by background evictor")
	}

	for i := 0; i < 3; i++ {
			_, ok, _ := cache.Lookup(fmt.Sprintf("fresh%d", i), "", nil)
		if !ok {
			t.Fatalf("fresh entry fresh%d should still exist", i)
		}
	}
}
