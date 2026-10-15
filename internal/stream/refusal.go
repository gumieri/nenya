package stream

import "strings"

// NENYA-191 — policy refusal / safeguard terminal classification.
//
// A provider can decline generated content two ways: an error payload worded
// as a content-policy/safeguard rejection, or a stream that terminates
// cleanly with a refusal-class stop/finish reason. Neither is a provider
// fault — the model (or its safety layer) declined the content — so the
// failover machinery must advance to the next target WITHOUT charging
// cooldown/breaker, but only while no client-visible output has been
// delivered: after content, the refusal terminal is the outcome (the client
// got the answer up to the refusal) and the request is never re-dispatched.
//
// Ordering contract for callers deciding on a buffered head (the NENYA-45
// bootstrap scan): effective output wins over refusal. A frame that both
// carries renderable output and names a refusal terminal means content was
// delivered — post-content semantics apply.

// refusalTerminalReasons are the stop/finish reasons that mark a policy
// refusal or content-filter stop. Compared lowercased: providers emit these
// as lowercase enums, and Gemini emits uppercase finishReason values.
var refusalTerminalReasons = map[string]bool{
	"refusal":            true, // Anthropic stop_reason; OpenAI finish_reason enum
	"content_filter":     true, // OpenAI finish_reason; the repo's normalized form
	"safeguard":          true, // provider safety-layer stop (LiteLLM #38606)
	"safety":             true, // Gemini finishReason (matched on any shape)
	"prohibited_content": true, // Gemini finishReason (matched on any shape)
	"blocklist":          true, // Gemini safety-class finishReason
	"spii":               true, // Gemini safety-class finishReason
	"image_safety":       true, // Gemini safety-class finishReason
	"recitation":         true, // Gemini safety-class finishReason
}

// IsRefusalStopReasonName reports whether a decoded stop/finish reason string
// names a policy refusal class (NENYA-191). It is the single reason list —
// the response-cache gate (NENYA-27) reuses it so refusal classification
// cannot drift between the failover and caching paths.
func IsRefusalStopReasonName(reason string) bool {
	return refusalTerminalReasons[strings.ToLower(strings.TrimSpace(reason))]
}

// policyRefusalMarkers are the phrases an upstream error payload uses when it
// is rejecting the content on policy grounds (NENYA-191). Deliberately
// multi-word: bare "moderation" or "policy" would match model names
// ("text-moderation-latest") and account-ban/quota wording, and misclassifying
// those skips the provider-fault charge the failure class deserves.
var policyRefusalMarkers = []string{
	"content policy",
	"content_policy",
	"content filter",
	"content_filter",
	"safeguard_error",
	"safety filter",
	"content moderation",
	"policy violation",
}

// IsPolicyRefusalText reports whether a bounded error text rejects the content
// on policy grounds (NENYA-191).
func IsPolicyRefusalText(text string) bool {
	if text == "" {
		return false
	}
	return containsAnyLower(text, policyRefusalMarkers)
}

// IsPolicyRefusalPayload reports whether a parsed upstream payload is an
// error-shaped rejection worded as a content-policy/safeguard refusal
// (NENYA-191). Only error-shaped payloads are inspected — a plain message
// frame is not a rejection (see IsStreamErrorPayload for the shape rules).
func IsPolicyRefusalPayload(parsed map[string]any) bool {
	if parsed == nil {
		return false
	}
	if !IsStreamErrorPayload(parsed) {
		return false
	}
	return IsPolicyRefusalText(rejectionText(parsed))
}

// IsRefusalTerminal reports whether a parsed upstream SSE payload terminates
// the stream with a policy refusal/content-filter/safeguard stop (NENYA-191).
// Recognized shapes:
//
//   - OpenAI chat: the FIRST choice's finish_reason (mirroring the
//     first-choice rule of the output test — later choices of an n>1 request
//     do not decide the stream);
//   - OpenAI-compat: the first choice's delta.stop_reason;
//   - Anthropic: delta.stop_reason (the message_delta event);
//   - Gemini: the FIRST candidate's finishReason;
//   - flat stop_reason / finish_reason.
//
// A terminal frame that also carries renderable output is NOT classified here
// by callers: delivered content makes the outcome terminal (post-content),
// so the output test must run first (see the package comment ordering
// contract).
func IsRefusalTerminal(parsed map[string]any) bool {
	if parsed == nil {
		return false
	}
	if openAIFirstChoiceRefuses(parsed) || geminiCandidatesRefuse(parsed) {
		return true
	}
	if delta, ok := parsed["delta"].(map[string]any); ok {
		if isRefusalReason(delta["stop_reason"]) {
			return true
		}
	}
	return isRefusalReason(parsed["stop_reason"]) || isRefusalReason(parsed["finish_reason"])
}

// openAIFirstChoiceRefuses reports whether the FIRST OpenAI-style choice
// terminates with a refusal-class finish_reason or delta stop_reason. The
// first choice is the one a client renders (the same rule as the output
// test), so a later choice's terminal must not decide the stream.
func openAIFirstChoiceRefuses(parsed map[string]any) bool {
	choices, ok := parsed["choices"].([]any)
	if !ok || len(choices) == 0 {
		return false
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return false
	}
	if isRefusalReason(choice["finish_reason"]) {
		return true
	}
	if delta, ok := choice["delta"].(map[string]any); ok {
		return isRefusalReason(delta["stop_reason"])
	}
	return false
}

// geminiCandidatesRefuse reports whether the FIRST Gemini candidate
// terminates with a refusal-class finishReason (SAFETY, PROHIBITED_CONTENT,
// BLOCKLIST, SPII, IMAGE_SAFETY, RECITATION) — the first candidate is the one
// a client renders, mirroring the output test's rule.
func geminiCandidatesRefuse(parsed map[string]any) bool {
	cands, ok := parsed["candidates"].([]any)
	if !ok || len(cands) == 0 {
		return false
	}
	cand, ok := cands[0].(map[string]any)
	if !ok {
		return false
	}
	return isRefusalReason(cand["finishReason"])
}

// isRefusalReason reports whether a stop/finish reason value names a
// policy refusal class (case-insensitive; shared normalization with
// IsRefusalStopReasonName).
func isRefusalReason(v any) bool {
	s, ok := v.(string)
	if !ok || s == "" {
		return false
	}
	return IsRefusalStopReasonName(s)
}

// containsAnyLower reports whether the lowercased text contains any marker.
func containsAnyLower(text string, markers []string) bool {
	lower := strings.ToLower(text)
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}
