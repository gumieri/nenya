package proxy

import (
	"context"
	"io"
	"strings"
	"testing"
)

// BenchmarkStallReader_Read_Pooled benchmarks readLoop pooling.
// Measures allocation reduction when reading from stallReader.
func BenchmarkStallReader_Read_Pooled(b *testing.B) {
	ctx := context.Background()
	chunk := strings.Repeat("x", 32*1024) // 32KB chunk

	b.ResetTimer()
	for range b.N {
		src := strings.NewReader(chunk)
		sr := newStallReader(ctx, src, 5*60)

		buf := make([]byte, 1024)
		for {
			_, err := sr.Read(buf)
			if err == io.EOF || err == nil {
				break
			}
			if err != nil {
				b.Fatalf("read failed: %v", err)
			}
		}
	}
}