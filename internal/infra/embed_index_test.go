package infra

import (
	"math"
	"testing"
)

func TestCosineSimilarity(t *testing.T) {
	// Test identical vectors
	a := []float32{1.0, 0.0, 0.0}
	b := []float32{1.0, 0.0, 0.0}
	sim := CosineSimilarity(a, b)
	if math.Abs(sim-1.0) > 1e-6 {
		t.Fatalf("Expected similarity 1.0 for identical vectors, got %f", sim)
	}

	// Test orthogonal vectors
	a = []float32{1.0, 0.0, 0.0}
	b = []float32{0.0, 1.0, 0.0}
	sim = CosineSimilarity(a, b)
	if math.Abs(sim-0.0) > 1e-6 {
		t.Fatalf("Expected similarity 0.0 for orthogonal vectors, got %f", sim)
	}

	// Test opposite vectors
	a = []float32{1.0, 0.0, 0.0}
	b = []float32{-1.0, 0.0, 0.0}
	sim = CosineSimilarity(a, b)
	if math.Abs(sim-(-1.0)) > 1e-6 {
		t.Fatalf("Expected similarity -1.0 for opposite vectors, got %f", sim)
	}

	// Test different length vectors (should return 0)
	a = []float32{1.0, 0.0}
	b = []float32{1.0, 0.0, 0.0}
	sim = CosineSimilarity(a, b)
	if sim != 0.0 {
		t.Fatalf("Expected similarity 0.0 for different length vectors, got %f", sim)
	}

	// Test zero vector
	a = []float32{0.0, 0.0, 0.0}
	b = []float32{1.0, 0.0, 0.0}
	sim = CosineSimilarity(a, b)
	if sim != 0.0 {
		t.Fatalf("Expected similarity 0.0 for zero vector, got %f", sim)
	}
}

func TestEmbedIndex(t *testing.T) {
	idx := NewEmbedIndex(3)

	// Insert some vectors
	idx.Insert("key1", []float32{1.0, 0.0, 0.0, 0.0})
	idx.Insert("key2", []float32{0.0, 1.0, 0.0, 0.0})
	idx.Insert("key3", []float32{0.0, 0.0, 1.0, 0.0})

	// Search for a vector similar to key1
	vec := []float32{0.9, 0.1, 0.0, 0.0}
	key, score, ok := idx.Search(vec, 0.8)
	if !ok {
		t.Fatal("Expected to find a similar vector")
	}
	if string(key) != "key1" {
		t.Fatalf("Expected key1, got %s", key)
	}
	if math.Abs(score-0.9899) > 0.01 {
		t.Fatalf("Expected score ~0.99, got %f", score)
	}

	// Search for a vector not similar to any
	vec = []float32{0.0, 0.0, 0.0, 0.5}
	_, _, ok = idx.Search(vec, 0.9)
	if ok {
		t.Fatal("Expected no match for dissimilar vector")
	}

	// Test with lower threshold that should match multiple - should return best match
	vec = []float32{0.6, 0.6, 0.0, 0.0}
	key, score, ok = idx.Search(vec, 0.5)
	if !ok {
		t.Fatal("Expected to find a similar vector")
	}
	// key1 cosine similarity: (0.6*1 + 0.6*0 + 0*0 + 0*0) / (norm([0.6,0.6,0,0]) * norm([1,0,0,0])) = 0.6 / 0.8485 = 0.7071
	// key2 cosine similarity: same as key1 (orthogonal within the 2D subspace)
	// Both have same similarity, but key1 was inserted first so it should be returned
	if string(key) != "key1" && string(key) != "key2" {
		t.Fatalf("Expected key1 or key2, got %s", key)
	}
	if math.Abs(score-0.707107) > 1e-5 {
		t.Fatalf("Expected score ~0.7071, got %f", score)
	}
}

func TestEmbedIndex_MaxVectorElements(t *testing.T) {
	idx := NewEmbedIndex(3)

	// Insert a vector with maximum allowed dimensions
	maxElements := make([]float32, 8192)
	for i := range maxElements {
		maxElements[i] = float32(i % 10) / 10.0
	}
	idx.Insert("max", maxElements)

	// Should be able to search with same dimensions
	key, _, ok := idx.Search(maxElements, 0.95)
	if !ok {
		t.Fatal("Should find exact match with max elements")
	}
	if string(key) != "max" {
		t.Fatalf("Expected 'max', got %s", key)
	}

	// Inserting a vector exceeding max elements should be rejected
	tooLarge := make([]float32, 8193)
	idx.Insert("too-large", tooLarge)

	// Search should still work, but "too-large" should not be stored
	_, _, ok = idx.Search(tooLarge, 0.95)
	if ok {
		t.Fatal("Should not find vector that exceeded element limit")
	}
}

func TestEmbedIndex_EmptyIndex(t *testing.T) {
	idx := NewEmbedIndex(3)

	vec := []float32{1.0, 0.0, 0.0}
	_, _, ok := idx.Search(vec, 0.5)
	if ok {
		t.Fatal("Empty index should return no match")
	}
}

func TestEmbedIndex_EmptyVectorSearch(t *testing.T) {
	idx := NewEmbedIndex(3)

	idx.Insert("key1", []float32{1.0, 0.0, 0.0})

	// Search with empty vector
	_, _, ok := idx.Search([]float32{}, 0.5)
	if ok {
		t.Fatal("Should not match empty search vector")
	}
}

func TestEmbedIndex_Remove(t *testing.T) {
	idx := NewEmbedIndex(10)

	idx.Insert("key1", []float32{1.0, 0.0, 0.0})
	idx.Insert("key2", []float32{0.0, 1.0, 0.0})
	idx.Insert("key3", []float32{0.0, 0.0, 1.0})

	// Remove middle entry
	idx.Remove("key2")

	// Verify removal
	_, _, ok := idx.Search([]float32{0.0, 1.0, 0.0}, 0.9)
	if ok {
		t.Fatal("Should not find removed entry")
	}

	// Verify other entries still exist
	_, _, ok = idx.Search([]float32{1.0, 0.0, 0.0}, 0.9)
	if !ok {
		t.Fatal("Should still find key1")
	}

	_, _, ok = idx.Search([]float32{0.0, 0.0, 1.0}, 0.9)
	if !ok {
		t.Fatal("Should still find key3")
	}
}

func TestEmbedIndex_MaxEntries(t *testing.T) {
	idx := NewEmbedIndex(3)

	// Insert 5 entries into index with max 3
	idx.Insert("key1", []float32{1.0, 0.0, 0.0})
	idx.Insert("key2", []float32{0.0, 1.0, 0.0})
	idx.Insert("key3", []float32{0.0, 0.0, 1.0})
	idx.Insert("key4", []float32{1.0, 1.0, 0.0})
	idx.Insert("key5", []float32{0.0, 1.0, 1.0})

	// Should have exactly 3 entries
	if idx.Len() != 3 {
		t.Fatalf("Expected 3 entries, got %d", idx.Len())
	}

	// First entries should be evicted
	_, _, ok := idx.Search([]float32{1.0, 0.0, 0.0}, 0.9)
	if ok {
		t.Fatal("key1 should have been evicted")
	}

	_, _, ok = idx.Search([]float32{0.0, 1.0, 0.0}, 0.9)
	if ok {
		t.Fatal("key2 should have been evicted")
	}

	// Last 3 entries should exist
	_, _, ok = idx.Search([]float32{0.0, 0.0, 1.0}, 0.9)
	if !ok {
		t.Fatal("key3 should still exist")
	}
	_, _, ok = idx.Search([]float32{1.0, 1.0, 0.0}, 0.9)
	if !ok {
		t.Fatal("key4 should still exist")
	}
	_, _, ok = idx.Search([]float32{0.0, 1.0, 1.0}, 0.9)
	if !ok {
		t.Fatal("key5 should still exist")
	}
}
