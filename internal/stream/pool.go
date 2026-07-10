package stream

import (
	"sync"
	"sync/atomic"
)

const (
	// maxPooledBufSize is the maximum buffer size we'll keep in the pool.
	// Buffers grown beyond this (e.g., scanner grew past 64KB) are discarded
	// to prevent pool memory bloat.
	maxPooledBufSize = 64 * 1024
)

var (
	sseBufPool sync.Pool

	poolHits   uint64
	poolMisses uint64
)

func init() {
	sseBufPool.New = func() interface{} {
		atomic.AddUint64(&poolMisses, 1)
		buf := make([]byte, SSEScannerInitialBuf)
		return &buf
	}
}

func getStreamBuffer() *[]byte {
	bufPtr := sseBufPool.Get()
	if bufPtr == nil {
		atomic.AddUint64(&poolMisses, 1)
		buf := make([]byte, SSEScannerInitialBuf)
		return &buf
	}
	atomic.AddUint64(&poolHits, 1)
	return bufPtr.(*[]byte)
}

func putStreamBuffer(buf *[]byte) {
	if buf == nil || *buf == nil {
		return
	}
	// Reject oversized buffers to prevent pool memory bloat
	if cap(*buf) > SSEScannerInitialBuf {
		return
	}
	sseBufPool.Put(buf)
}

func GetPoolStats() map[string]uint64 {
	return map[string]uint64{
		"hits":   atomic.LoadUint64(&poolHits),
		"misses": atomic.LoadUint64(&poolMisses),
	}
}

// lineCopyPool is a sync.Pool for line-copy scratch buffers used by
// SSETransformingReader. These buffers replace per-read `make([]byte, n)`
// allocations in the streaming hot path.
var lineCopyPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, SSEScannerInitialBuf)
		return &buf
	},
}

// GetLineCopyBuffer returns a pooled scratch buffer for line copies.
// The returned buffer has len=0, cap=SSEScannerInitialBuf.
func GetLineCopyBuffer() *[]byte {
	return lineCopyPool.Get().(*[]byte)
}

// PutLineCopyBuffer returns a line-copy buffer to the pool.
// Buffers whose capacity exceeds maxPooledBufSize are discarded to prevent
// pool memory bloat.
func PutLineCopyBuffer(buf *[]byte) {
	if buf == nil || *buf == nil {
		return
	}
	if cap(*buf) > maxPooledBufSize {
		return
	}
	*buf = (*buf)[:0]
	lineCopyPool.Put(buf)
}
