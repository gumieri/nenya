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
		"model":    "gpt-4",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	}

	withStream := map[string]interface{}{
		"model":    "gpt-4",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"stream":   true,
	}

	withUser := map[string]interface{}{
		"model":    "gpt-4",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"user":     "test-user-123",
	}

	withSeed := map[string]interface{}{
		"model":    "gpt-4",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"seed":     42,
	}

	hBase := FingerprintPayload(base)
	if FingerprintPayload(withStream) == hBase {
		t.Fatal("stream field should affect fingerprint (different stream mode)")
	}
	if FingerprintPayload(withUser) != hBase {
		t.Fatal("user field should not affect fingerprint")
	}
	if FingerprintPayload(withSeed) != hBase {
		t.Fatal("seed field should not affect fingerprint")
	}
}

func TestFingerprintPayload_DifferentContent(t *testing.T) {
	p1 := map[string]interface{}{
		"model":    "gpt-4",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hello"}},
	}
	p2 := map[string]interface{}{
		"model":    "gpt-4",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "goodbye"}},
	}
	h1 := FingerprintPayload(p1)
	h2 := FingerprintPayload(p2)
	if h1 == h2 {
		t.Fatal("different messages should produce different fingerprints")
	}
}

func TestFingerprintPayload_DifferentModel(t *testing.T) {
	p1 := map[string]interface{}{
		"model":    "gpt-4",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	}
	p2 := map[string]interface{}{
		"model":    "claude-3",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	}
	h1 := FingerprintPayload(p1)
	h2 := FingerprintPayload(p2)
	if h1 == h2 {
		t.Fatal("different models should produce different fingerprints")
	}
}

func TestFingerprintPayload_ToolsAndStop(t *testing.T) {
	p1 := map[string]interface{}{
		"model":    "gpt-4",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"tools":    []interface{}{map[string]interface{}{"type": "function"}},
		"stop":     []interface{}{"END"},
	}

	p2 := map[string]interface{}{
		"model":    "gpt-4",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	}

	h1 := FingerprintPayload(p1)
	h2 := FingerprintPayload(p2)
	if h1 == h2 {
		t.Fatal("payload with tools/stop should differ from base")
	}
}

func TestFingerprintPayload_ToolCallMessages(t *testing.T) {
	base := map[string]interface{}{
		"model": "gpt-4",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
		},
	}

	withToolCalls := map[string]interface{}{
		"model": "gpt-4",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
			map[string]interface{}{
				"role":    "assistant",
				"content": "",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":   "call_123",
						"type": "function",
						"function": map[string]interface{}{
							"name":      "read_file",
							"arguments": `{"path":"/tmp/test.go"}`,
						},
					},
				},
			},
			map[string]interface{}{
				"role":         "tool",
				"tool_call_id": "call_123",
				"content":      "file contents",
			},
		},
	}

	withTools := map[string]interface{}{
		"model": "gpt-4",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
		},
		"tools": []interface{}{
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "read_file",
					"description": "Read a file",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"path": map[string]interface{}{"type": "string"},
						},
					},
				},
			},
		},
		"tool_choice": "auto",
	}

	hBase := FingerprintPayload(base)
	hToolCalls := FingerprintPayload(withToolCalls)
	hTools := FingerprintPayload(withTools)

	if hToolCalls == hBase {
		t.Fatal("messages with tool_calls should differ from base")
	}
	if hTools == hBase {
		t.Fatal("payload with tools/tool_choice should differ from base")
	}
	if hToolCalls == hTools {
		t.Fatal("tool_calls in messages vs tools param should differ")
	}

	hToolCallsAgain := FingerprintPayload(withToolCalls)
	if hToolCalls != hToolCallsAgain {
		t.Fatal("same tool_calls payload should produce same fingerprint")
	}

	differentToolCall := map[string]interface{}{
		"model": "gpt-4",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
			map[string]interface{}{
				"role":    "assistant",
				"content": "",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":   "call_456",
						"type": "function",
						"function": map[string]interface{}{
							"name":      "write_file",
							"arguments": `{"path":"/tmp/out.go"}`,
						},
					},
				},
			},
			map[string]interface{}{
				"role":         "tool",
				"tool_call_id": "call_456",
				"content":      "ok",
			},
		},
	}
	if FingerprintPayload(differentToolCall) == hToolCalls {
		t.Fatal("different tool_calls should produce different fingerprints")
	}
}

func TestFingerprintPayload_EmptyPayload(t *testing.T) {
	h := FingerprintPayload(map[string]interface{}{})
	if h == "" {
		t.Fatal("empty payload should still produce a fingerprint")
	}
}

func TestResponseCache_StoreAndLookup(t *testing.T) {
	cache := NewResponseCache(10, 1<<20, 1*time.Hour, 1*time.Hour)
	defer cache.Stop()

	cache.Store("key1", []byte("response1"))
	cache.Store("key2", []byte("response2"))

	data, ok := cache.Lookup("key1")
	if !ok || string(data) != "response1" {
		t.Fatalf("expected response1, got %q, ok=%v", data, ok)
	}

	data, ok = cache.Lookup("key2")
	if !ok || string(data) != "response2" {
		t.Fatalf("expected response2, got %q, ok=%v", data, ok)
	}

	_, ok = cache.Lookup("nonexistent")
	if ok {
		t.Fatal("nonexistent key should not be found")
	}
}

func TestResponseCache_TTLExpiration(t *testing.T) {
	cache := NewResponseCache(10, 1<<20, 100*time.Millisecond, 50*time.Millisecond)
	defer cache.Stop()

	cache.Store("key1", []byte("response1"))

	data, ok := cache.Lookup("key1")
	if !ok || string(data) != "response1" {
		t.Fatal("should find entry immediately")
	}

	time.Sleep(150 * time.Millisecond)

	_, ok = cache.Lookup("key1")
	if ok {
		t.Fatal("expired entry should not be found")
	}

	if cache.Len() != 0 {
		t.Fatalf("expected 0 entries after expiration, got %d", cache.Len())
	}
}

func TestResponseCache_LRU_Eviction(t *testing.T) {
	cache := NewResponseCache(3, 1<<20, 1*time.Hour, 1*time.Hour)
	defer cache.Stop()

	cache.Store("key1", []byte("a"))
	cache.Store("key2", []byte("b"))
	cache.Store("key3", []byte("c"))

	if cache.Len() != 3 {
		t.Fatalf("expected 3 entries, got %d", cache.Len())
	}

	cache.Store("key4", []byte("d"))

	if cache.Len() != 3 {
		t.Fatalf("expected 3 entries after eviction, got %d", cache.Len())
	}

	_, ok := cache.Lookup("key1")
	if ok {
		t.Fatal("oldest entry (key1) should have been evicted")
	}

	data, ok := cache.Lookup("key2")
	if !ok || string(data) != "b" {
		t.Fatal("key2 should still exist")
	}

	data, ok = cache.Lookup("key4")
	if !ok || string(data) != "d" {
		t.Fatal("key4 should exist")
	}
}

func TestResponseCache_MaxEntryBytes(t *testing.T) {
	cache := NewResponseCache(10, 100, 1*time.Hour, 1*time.Hour)
	defer cache.Stop()

	cache.Store("small", []byte("hello"))
	data, ok := cache.Lookup("small")
	if !ok || string(data) != "hello" {
		t.Fatal("small entry should be stored")
	}

	largeData := strings.Repeat("x", 200)
	cache.Store("large", []byte(largeData))

	_, ok = cache.Lookup("large")
	if ok {
		t.Fatal("oversized entry should not be stored")
	}

	if cache.Len() != 1 {
		t.Fatalf("expected 1 entry, got %d", cache.Len())
	}
}

func TestResponseCache_StoreUpdate(t *testing.T) {
	cache := NewResponseCache(10, 1<<20, 1*time.Hour, 1*time.Hour)
	defer cache.Stop()

	cache.Store("key1", []byte("v1"))
	cache.Store("key1", []byte("v2"))

	if cache.Len() != 1 {
		t.Fatalf("expected 1 entry after update, got %d", cache.Len())
	}

	data, ok := cache.Lookup("key1")
	if !ok || string(data) != "v2" {
		t.Fatalf("expected v2, got %q", data)
	}
}

func TestResponseCache_EmptyKeyAndData(t *testing.T) {
	cache := NewResponseCache(10, 1<<20, 1*time.Hour, 1*time.Hour)
	defer cache.Stop()

	cache.Store("", []byte("data"))
	if cache.Len() != 0 {
		t.Fatal("empty key should not be stored")
	}

	cache.Store("key", nil)
	if cache.Len() != 0 {
		t.Fatal("nil data should not be stored")
	}

	cache.Store("key", []byte{})
	if cache.Len() != 0 {
		t.Fatal("empty data should not be stored")
	}
}

func TestResponseCache_BackgroundEvictor(t *testing.T) {
	cache := NewResponseCache(100, 1<<20, 200*time.Millisecond, 100*time.Millisecond)
	defer cache.Stop()

	for i := 0; i < 50; i++ {
		cache.Store(fmt.Sprintf("key%d", i), []byte(fmt.Sprintf("val%d", i)))
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
	cache := NewResponseCache(10, 1<<20, 1*time.Hour, 50*time.Millisecond)

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
	cache := NewResponseCache(100, 1<<20, 10*time.Minute, 1*time.Hour)
	defer cache.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("key%d", idx)
			cache.Store(key, []byte(fmt.Sprintf("val%d", idx)))
			time.Sleep(time.Microsecond * time.Duration(idx%5))
			cache.Lookup(key)
			cache.Lookup("nonexistent")
		}(i)
	}
	wg.Wait()

	expected := 100
	if cache.Len() != expected {
		t.Fatalf("expected %d entries after concurrent access, got %d", expected, cache.Len())
	}
}

func TestResponseCache_BackgroundEvictorRemovesExpiredFirst(t *testing.T) {
	cache := NewResponseCache(5, 1<<20, 50*time.Millisecond, 20*time.Millisecond)
	defer cache.Stop()

	cache.Store("expires-soon", []byte("a"))
	cache.Store("expires-soon-2", []byte("b"))

	time.Sleep(70 * time.Millisecond)

	for i := 0; i < 3; i++ {
		cache.Store(fmt.Sprintf("fresh%d", i), []byte(fmt.Sprintf("f%d", i)))
	}

	time.Sleep(40 * time.Millisecond)

	_, ok := cache.Lookup("expires-soon")
	if ok {
		t.Fatal("expired entries should be cleaned up by background evictor")
	}
	_, ok = cache.Lookup("expires-soon-2")
	if ok {
		t.Fatal("expired entries should be cleaned up by background evictor")
	}

	for i := 0; i < 3; i++ {
		_, ok := cache.Lookup(fmt.Sprintf("fresh%d", i))
		if !ok {
			t.Fatalf("fresh entry fresh%d should still exist", i)
		}
	}
}
