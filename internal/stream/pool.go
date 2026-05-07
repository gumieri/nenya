package stream

import (
	"sync"
	"sync/atomic"
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
	if buf != nil && *buf != nil {
		sseBufPool.Put(buf)
	}
}

func GetPoolStats() map[string]uint64 {
	return map[string]uint64{
		"hits":   atomic.LoadUint64(&poolHits),
		"misses": atomic.LoadUint64(&poolMisses),
	}
}
