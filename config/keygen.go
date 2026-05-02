package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
)

func GenerateToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("failed to read random: " + err.Error())
	}
	return "nk-" + hex.EncodeToString(b)
}

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
