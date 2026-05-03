package util

import "runtime"

// ZeroBytes overwrites the contents of b with zero bytes.
// It prevents sensitive data from lingering in memory after use.
// This is a no‑op if b is nil.
func ZeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
	runtime.KeepAlive(b)
}
