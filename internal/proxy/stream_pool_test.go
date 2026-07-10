package proxy

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// TestStallReaderReadLoopPool_NoAliasing verifies that remainBuf does not alias
// pool buffers after a partial read. A regression test for a bug where
// remainBuf = rr.data[n:] created an alias to a pool buffer that was
// returned to the pool, causing concurrent read/write.
func TestStallReaderReadLoopPool_NoAliasing(t *testing.T) {
	ctx := context.Background()

	// Simulate a partial read scenario:
	// 1. readLoop sends pool-backed data via channel
	// 2. Read copies partial to p, stores remainder in remainBuf
	// 3. Read should copy remainBuf (not alias), then return poolBuf

	// Create a large chunk that will trigger remainBuf
	largeChunk := strings.Repeat("x", 64*1024) // 64KB
	src := io.MultiReader(
		strings.NewReader(largeChunk),
		strings.NewReader("part2"),
	)
	sr := newStallReader(ctx, src, 5*time.Second)
	defer sr.Stop() // Ensure timer and goroutine are cleaned up

	// Read in small chunks to trigger remainBuf path
	buf := make([]byte, 128) // smaller than chunk size

	// First read: partial, triggers remainBuf
	n, err := sr.Read(buf)
	if err != nil {
		t.Fatalf("first read failed: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("expected %d bytes, got %d", len(buf), n)
	}

	// Verify remainBuf exists
	sr.mu.Lock()
	remainBufLen := len(sr.remainBuf)
	sr.mu.Unlock()
	if remainBufLen == 0 {
		t.Fatal("expected remainBuf after partial read, got empty")
	}

	// Read all remaining data
	var allRead []byte
	allRead = append(allRead, buf...)

	for {
		nn, err := sr.Read(buf)
		if err == io.EOF || err == nil && nn == 0 {
			break
		}
		if err != nil {
			t.Fatalf("read failed: %v", err)
		}
		allRead = append(allRead, buf[:nn]...)
	}

	// Verify no data corruption by comparing with expected output
	expected := largeChunk + "part2"
	if !bytes.Equal(allRead, []byte(expected)) {
		t.Fatalf("data corruption:\nexpected length %d\ngot length %d\nfirst 100 bytes: %q", len(expected), len(allRead), allRead[:min(100, len(allRead))])
	}
}

// TestStallReaderReadLoopPool_MultipleChunks verifies pooling works
// correctly across multiple chunks from the channel.
func TestStallReaderReadLoopPool_MultipleChunks(t *testing.T) {
	ctx := context.Background()

	// Create multiple chunks
	chunks := []string{
		strings.Repeat("a", 1024),
		strings.Repeat("b", 1024),
		strings.Repeat("c", 1024),
	}
	src := io.MultiReader(
		strings.NewReader(chunks[0]),
		strings.NewReader(chunks[1]),
		strings.NewReader(chunks[2]),
	)
	sr := newStallReader(ctx, src, 5*time.Second)
	defer sr.Stop() // Ensure timer and goroutine are cleaned up

	// Read all data
	var allRead []byte
	buf := make([]byte, 512) // smaller than chunk size to trigger remainBuf

	for {
		n, err := sr.Read(buf)
		if err == io.EOF || err == nil && n == 0 {
			break
		}
		if err != nil {
			t.Fatalf("read failed: %v", err)
		}
		allRead = append(allRead, buf[:n]...)
	}

	// Verify data integrity
	expected := strings.Join(chunks, "")
	if !bytes.Equal(allRead, []byte(expected)) {
		t.Fatalf("data corruption:\nexpected length %d\ngot length %d", len(expected), len(allRead))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}