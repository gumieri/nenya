package util

// ThoughtSignaturePlaceholder is the value Gemini 3+ tolerates on unsigned
// tool calls instead of rejecting them with a 400: injecting it as the
// thought signature tells the upstream to skip signature validation for
// that call (GoModel #894 fix 2).
const ThoughtSignaturePlaceholder = "skip_thought_signature_validator"

// thoughtSignatureFlatKeys are the flat spellings Google's own
// OpenAI-compat surface uses on tool_calls. Nenya's canonical form is
// extra_content; clients echoing the flat keys would otherwise look
// unsigned to the signature policy and lose their paired history
// (NENYA-50).
var thoughtSignatureFlatKeys = []string{"thought_signature", "thoughtSignature"}

// NormalizeFlatThoughtSignature moves flat thought-signature spellings on a
// tool_call into the canonical extra_content form:
//
//	extra_content.google.thought_signature
//
// Returns true when a flat key was found and normalized. Safe to call on
// calls that already carry extra_content (flat keys are still removed so
// upstream sees exactly one signature).
func NormalizeFlatThoughtSignature(tc map[string]interface{}) bool {
	moved := false
	for _, key := range thoughtSignatureFlatKeys {
		sig, ok := tc[key].(string)
		if !ok || sig == "" {
			continue
		}
		delete(tc, key)
		if moved {
			continue // keep the first signature, drop redundant echoes
		}
		ensureExtraContentMap(tc)["google"] = map[string]interface{}{
			"thought_signature": sig,
		}
		moved = true
	}
	return moved
}

// EnsureThoughtSignaturePlaceholder injects the upstream-tolerated skip
// placeholder for an unsigned tool call. Returns true when a placeholder
// was added (calls with any existing signature are untouched).
func EnsureThoughtSignaturePlaceholder(tc map[string]interface{}) bool {
	if hasThoughtSignature(tc) {
		return false
	}
	ensureExtraContentMap(tc)["google"] = map[string]interface{}{
		"thought_signature": ThoughtSignaturePlaceholder,
	}
	return true
}

// HasThoughtSignature reports whether the tool_call carries a signature in
// canonical extra_content form (any shape counts: the strip policy only
// needs to know the call is signed, not which surface spelled it).
func HasThoughtSignature(tc map[string]interface{}) bool {
	return hasThoughtSignature(tc)
}

func hasThoughtSignature(tc map[string]interface{}) bool {
	extra, ok := tc["extra_content"].(map[string]interface{})
	if !ok {
		return false
	}
	google, ok := extra["google"].(map[string]interface{})
	if !ok {
		return false
	}
	sig, ok := google["thought_signature"].(string)
	return ok && sig != ""
}

func ensureExtraContentMap(tc map[string]interface{}) map[string]interface{} {
	if extra, ok := tc["extra_content"].(map[string]interface{}); ok {
		return extra
	}
	extra := make(map[string]interface{}, 1)
	tc["extra_content"] = extra
	return extra
}
