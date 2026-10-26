package util

import "unicode/utf8"

// RuneSafeTruncate caps s at max bytes, cutting on a UTF-8 rune boundary so
// the result is always valid UTF-8 (a raw byte slice would split multi-byte
// runes and log invalid fragments for non-ASCII input). max <= 0 returns the
// empty string; s within max is returned unchanged.
func RuneSafeTruncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
