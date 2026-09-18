package util

import (
	"strconv"
	"strings"
)

// SupportsMidConversationSystem reports whether the model family accepts
// role=system messages positioned after the first conversation turn.
// Anthropic introduced mid-conversation system support in the 4.8
// generation; every later major version inherits it. Model IDs are
// segmented (provider-qualified IDs allowed), the Anthropic segment is
// located, and its version is parsed from the numeric tokens following
// the "claude" token: major > 4, or major == 4 with minor >= 8.
//
// Lives in util (not discovery) because the Anthropic adapter needs the
// predicate and discovery already imports adapter.
func SupportsMidConversationSystem(modelID string) bool {
	for _, seg := range strings.Split(strings.ToLower(modelID), "/") {
		major, minor, ok := claudeVersion(seg)
		if !ok {
			continue
		}
		return major > 4 || (major == 4 && minor >= 8)
	}
	return false
}

// claudeVersion extracts (major, minor) from a model segment whose ID
// carries an Anthropic claude generation, e.g. "claude-sonnet-4-8" or
// "claude-4-5-sonnet". Returns ok=false for non-claude segments or
// segments without a parseable version.
func claudeVersion(seg string) (major, minor int, ok bool) {
	if !strings.HasPrefix(seg, "claude") {
		return 0, 0, false
	}
	foundClaude := false
	sawMajor := false
	for _, part := range strings.Split(seg, "-") {
		if part == "claude" {
			foundClaude = true
			continue
		}
		if !foundClaude {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			continue // family token (sonnet, opus, haiku, ...)
		}
		if !sawMajor {
			major = n
			sawMajor = true
			continue
		}
		minor = n
		return major, minor, true
	}
	if sawMajor {
		// Version has no minor component (e.g. "claude-sonnet-5").
		return major, 0, true
	}
	return 0, 0, false
}
