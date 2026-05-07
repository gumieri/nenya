package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
)

// GenerateToken generates a cryptographically random API token with an
// "nk-" prefix. Uses crypto/rand for secure randomness. The token is 48
// hex characters (24 bytes) plus the prefix.
func GenerateToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("failed to read random: " + err.Error())
	}
	return "nk-" + hex.EncodeToString(b)
}

// ValidateKeyID validates an API key ID string: must be non-empty, at
// most 64 characters, and contain only lowercase letters, digits, and
// hyphens.
func ValidateKeyID(id string) error {
	if id == "" {
		return fmt.Errorf("key ID cannot be empty")
	}
	if len(id) > 64 {
		return fmt.Errorf("key ID too long (max 64 chars)")
	}
	if !regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(id) {
		return fmt.Errorf("key ID must contain only lowercase letters, digits, and hyphens")
	}
	return nil
}
