package util

import (
	"regexp"
	"strconv"
	"strings"
)

// vertexSuffixRe strips Vertex-style suffixes like @default.
var vertexSuffixRe = regexp.MustCompile(`@[a-z]+$`)

// AnthropicVersion extracts the model family (opus, sonnet, haiku), major version,
// and minor version from an Anthropic model ID. Returns (family, major, minor, ok).
// Handles these naming conventions:
// - Standard: "claude-opus-4-7", "claude-sonnet-5"
// - Vertex: "claude-opus-4-8@default" (with @ suffix)
// - SAP: "claude-4.7-opus" (inverted family position)
// - With variant: "claude-sonnet-4-6-20250505"
func AnthropicVersion(model string) (family string, major, minor int, ok bool) {
	model = vertexSuffixRe.ReplaceAllString(model, "")
	parts := strings.Split(model, "-")
	if len(parts) < 3 || parts[0] != "claude" {
		return "", 0, 0, false
	}

	if f, m, mn, found := parseSAPInverted(parts); found {
		return f, m, mn, true
	}

	return parseStandard(parts)
}

// parseSAPInverted handles the "claude-4.7-opus" naming convention where the
// version comes before the family name.
func parseSAPInverted(parts []string) (family string, major, minor int, ok bool) {
	if len(parts) < 3 {
		return "", 0, 0, false
	}
	// SAP format: claude-4.7-opus OR claude-5.0-sonnet (version in second part)
	versionPart := parts[1]
	versionParts := strings.Split(versionPart, ".")
	major, err := strconv.Atoi(versionParts[0])
	if err != nil {
		return "", 0, 0, false
	}
	if len(versionParts) >= 2 {
		minor, _ = strconv.Atoi(versionParts[1])
	}
	fam := parts[len(parts)-1]
	if !isValidAnthropicFamily(fam) {
		return "", 0, 0, false
	}
	return fam, major, minor, true
}

// parseStandard handles the "claude-opus-4-7" naming convention.
func parseStandard(parts []string) (family string, major, minor int, ok bool) {
	fam := parts[1]
	if !isValidAnthropicFamily(fam) {
		return "", 0, 0, false
	}
	if len(parts) >= 4 {
		m, err1 := strconv.Atoi(parts[2])
		min, err2 := strconv.Atoi(parts[3])
		if err1 == nil && err2 == nil {
			return fam, m, min, true
		}
	}
	if len(parts) >= 3 {
		if m, err := strconv.Atoi(parts[2]); err == nil {
			return fam, m, 0, true
		}
	}
	return "", 0, 0, false
}

func isValidAnthropicFamily(fam string) bool {
	return fam == "opus" || fam == "sonnet" || fam == "haiku"
}

// IsAnthropicAtLeast checks if the model is from the specified family and has a
// version >= (major, minor). Minor is treated as 0 if omitted in the model ID.
func IsAnthropicAtLeast(model string, wantFamily string, wantMajor, wantMinor int) bool {
	family, major, minor, ok := AnthropicVersion(model)
	if !ok || family != wantFamily {
		return false
	}
	if major > wantMajor {
		return true
	}
	return major == wantMajor && minor >= wantMinor
}

// IsAnthropicOpus47OrLater returns true if the model is an Opus model with
// version >= 4.7. This is the gate for adaptive thinking support.
func IsAnthropicOpus47OrLater(model string) bool {
	return IsAnthropicAtLeast(model, "opus", 4, 7)
}

// IsAnthropicSonnet5OrLater returns true if the model is a Sonnet model with
// version >= 5.0. This is the gate for adaptive thinking support.
func IsAnthropicSonnet5OrLater(model string) bool {
	return IsAnthropicAtLeast(model, "sonnet", 5, 0)
}
