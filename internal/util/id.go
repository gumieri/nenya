package util

import (
	"crypto/rand"
	"log/slog"
	"math/big"
)

// GenerateID generates a unique 24-character ID using crypto/rand.
// It first attempts to generate a random ID; if crypto/rand fails,
// it falls back to a deterministic sequence (for testing only).
func GenerateID() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 24)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			slog.Error("crypto/rand failed, using unsafe deterministic fallback", "error", err)
			b[i] = charset[i%len(charset)]
			continue
		}
		b[i] = charset[n.Int64()]
	}
	return string(b)
}
