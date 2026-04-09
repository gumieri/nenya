package infra

import (
	"testing"
	"time"
)

func TestThoughtSignatureCache_StoreAndLoad(t *testing.T) {
	cache := NewThoughtSignatureCache(100, 5*time.Minute)

	cache.Store("tc-1", map[string]interface{}{"google": map[string]string{"thought_signature": "abc"}})
	cache.Store("tc-2", map[string]interface{}{"google": map[string]string{"thought_signature": "def"}})

	val, ok := cache.Load("tc-1")
	if !ok {
		t.Fatal("expected tc-1 to be found")
	}
	sig, _ := val.(map[string]interface{})
	google, _ := sig["google"].(map[string]string)
	if google["thought_signature"] != "abc" {
		t.Errorf("expected abc, got %s", google["thought_signature"])
	}

	_, ok = cache.Load("tc-2")
	if !ok {
		t.Fatal("expected tc-2 to be found")
	}

	_, ok = cache.Load("tc-nonexistent")
	if ok {
		t.Error("expected tc-nonexistent not to be found")
	}
}

func TestThoughtSignatureCache_Expiration(t *testing.T) {
	cache := NewThoughtSignatureCache(100, 50*time.Millisecond)

	cache.Store("expiring", "value")
	val, ok := cache.Load("expiring")
	if !ok || val != "value" {
		t.Fatal("expected value immediately")
	}

	time.Sleep(100 * time.Millisecond)

	_, ok = cache.Load("expiring")
	if ok {
		t.Error("expected entry to be expired")
	}
}

func TestThoughtSignatureCache_Eviction(t *testing.T) {
	cache := NewThoughtSignatureCache(3, 5*time.Minute)

	cache.Store("a", 1)
	cache.Store("b", 2)
	cache.Store("c", 3)
	if cache.Len() != 3 {
		t.Fatalf("expected 3 entries, got %d", cache.Len())
	}

	cache.Store("d", 4)
	cache.Store("e", 5)

	time.Sleep(10 * time.Millisecond)
	if cache.Len() > 4 {
		t.Errorf("expected eviction to reduce size, got %d", cache.Len())
	}
}

func TestThoughtSignatureCache_NilAndEmptyKey(t *testing.T) {
	cache := NewThoughtSignatureCache(100, time.Minute)

	cache.Store("", "value")
	if _, ok := cache.Load(""); ok {
		t.Error("expected empty key to be rejected")
	}

	cache.Store("key", nil)
	if _, ok := cache.Load("key"); ok {
		t.Error("expected nil value to be rejected")
	}
}

func TestThoughtSignatureCache_Len(t *testing.T) {
	cache := NewThoughtSignatureCache(100, time.Minute)
	if cache.Len() != 0 {
		t.Errorf("expected 0, got %d", cache.Len())
	}

	cache.Store("a", 1)
	cache.Store("b", 2)
	if cache.Len() != 2 {
		t.Errorf("expected 2, got %d", cache.Len())
	}
}

func TestThoughtSignatureCache_OverwriteExisting(t *testing.T) {
	cache := NewThoughtSignatureCache(100, time.Minute)
	cache.Store("key", "old")
	cache.Store("key", "new")

	val, ok := cache.Load("key")
	if !ok || val != "new" {
		t.Error("expected overwritten value")
	}
}

func TestThoughtSignatureCache_ZeroDefaults(t *testing.T) {
	cache := NewThoughtSignatureCache(0, 0)
	if cache.Len() != 0 {
		t.Errorf("expected 0, got %d", cache.Len())
	}
	cache.Store("a", 1)
	val, ok := cache.Load("a")
	if !ok || val != 1 {
		t.Error("expected default-sized cache to work")
	}
}
