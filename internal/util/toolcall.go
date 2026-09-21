package util

import (
	"fmt"
	"strconv"
	"strings"
)

// ToolCallIDPrefix is the stable prefix for gateway-synthesized tool-call
// IDs. One constant so every generation/inspection site agrees (NENYA-48).
const ToolCallIDPrefix = "call_"

// maxToolCallIDLen bounds accepted upstream tool-call IDs; anything longer
// is replaced with a deterministic synthetic ID. 128 is generous versus
// upstream practice (OpenAI ~40, Anthropic ~30) while preventing memory
// abuse from adversarial payloads.
const maxToolCallIDLen = 128

// SyntheticToolCallID deterministically derives a tool-call ID from an
// index. Same index → same ID across requests: clients and signature
// caches key replay on these IDs, so generation must never be random.
func SyntheticToolCallID(index int) string {
	return fmt.Sprintf("%s%d", ToolCallIDPrefix, index)
}

// IsSyntheticToolCallID reports whether an ID was gateway-synthesized
// (matches the stable prefix). Used by merge heuristics that must treat
// synthetic IDs as placeholder-grade rather than authoritative.
func IsSyntheticToolCallID(id string) bool {
	return strings.HasPrefix(id, ToolCallIDPrefix)
}

// ValidToolCallID reports whether an upstream-provided tool-call ID is
// usable as-is: a non-empty string within the length bound. Invalid IDs
// are replaced with SyntheticToolCallID(index) by the normalization sites.
func ValidToolCallID(id interface{}) bool {
	s, ok := id.(string)
	if !ok {
		return false
	}
	return s != "" && len(s) <= maxToolCallIDLen
}

// ParseToolCallIndex extracts a non-negative integer index from an
// interface value, returning fallback for missing or invalid values.
func ParseToolCallIndex(v interface{}, fallback int) int {
	i, err := strconv.Atoi(fmt.Sprintf("%v", v))
	if err != nil || i < 0 {
		return fallback
	}
	return i
}
