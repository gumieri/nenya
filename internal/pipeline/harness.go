package pipeline

import (
	"regexp"
	"strings"
)

// Harness envelopes are client-side scaffolding that coding-agent harnesses
// (OpenCode, Claude Code, Aider) splice into their own system/user messages:
// environment dumps, command echoes, and system reminders. Their text is
// imperative ("NEVER ...", "MUST ...") by design, so the deterministic
// injection scanner flags it — and the tier-2 classifier then judges harness
// scaffolding as if it were user content, wasting budget and risking a false
// verdict on text the user never wrote (NENYA-186, mirroring the LiteLLM
// #40599/#40655 class).
//
// Detection is deliberately conservative and structural. A surface is
// scaffolding only when its ENTIRE content is one or more well-formed
// envelopes (plus whitespace), it arrives as a client-authored plain-string
// `system`/`user` message (never tool output or a multimodal array), and every
// detection on it is an imperative-wording category — an envelope carrying
// hidden-text, encoded, or custom-detector hits stays fully actionable. A
// lone `<system-reminder>` smuggled into fetched content therefore remains
// ordinary untrusted text, and every exemption taken is recorded (kind counter
// + injection "cleared"/"harness" metric), never silent.

type harnessEnvelopeKind string

const (
	harnessKindSystemReminder harnessEnvelopeKind = "system_reminder"
	harnessKindEnv            harnessEnvelopeKind = "env"
	harnessKindCommand        harnessEnvelopeKind = "command"
	harnessKindGateway        harnessEnvelopeKind = "gateway_marker"
	harnessKindOther          harnessEnvelopeKind = "other"
)

// harnessEnvelopeTags maps a well-formed paired envelope to its kind. Paired
// tags are matched open→close with no nesting (harnesses do not nest these),
// and the open tag must be exactly the tag (no attributes): an unknown
// attribute keeps the surface untrusted rather than exempting it.
var harnessEnvelopeTags = []struct {
	kind  harnessEnvelopeKind
	open  string
	close string
}{
	{harnessKindSystemReminder, "<system-reminder>", "</system-reminder>"},
	{harnessKindEnv, "<env>", "</env>"},
	{harnessKindCommand, "<command-name>", "</command-name>"},
	{harnessKindCommand, "<command-message>", "</command-message>"},
	{harnessKindCommand, "<local-command-stdout>", "</local-command-stdout>"},
	{harnessKindCommand, "<local-command-caveat>", "</local-command-caveat>"},
	{harnessKindCommand, "<command-args>", "</command-args>"},
	{harnessKindOther, "<additional_tools>", "</additional_tools>"},
}

// gatewayCanaryMarkerRe matches the gateway's own canary trailer exactly:
// CanaryMarker = "\n[internal-ref] NENYA-CANARY-<32 hex> [do not modify]".
// The structured match matters: a prefix-only test would exempt any message
// that merely starts with the literal (NENYA-186 review).
var gatewayCanaryMarkerRe = regexp.MustCompile(
	`^\[internal-ref\] NENYA-CANARY-[0-9a-f]{32} \[do not modify\]$`)

// classifyHarnessEnvelope reports whether text is entirely harness scaffolding
// and, if so, which kind (the first envelope's kind). Whitespace between and
// around envelopes is allowed.
func classifyHarnessEnvelope(text string) (harnessEnvelopeKind, bool) {
	rest := strings.TrimSpace(text)
	if rest == "" {
		return "", false
	}
	if gatewayCanaryMarkerRe.MatchString(rest) {
		return harnessKindGateway, true
	}
	first := harnessEnvelopeKind("")
	for rest != "" {
		kind, span, ok := nextHarnessSpan(rest)
		if !ok {
			return "", false
		}
		if first == "" {
			first = kind
		}
		rest = strings.TrimSpace(rest[span:])
	}
	return first, true
}

// nextHarnessSpan returns the kind and byte length of the harness envelope at
// the start of text, if any.
func nextHarnessSpan(text string) (harnessEnvelopeKind, int, bool) {
	for _, tag := range harnessEnvelopeTags {
		if !strings.HasPrefix(text, tag.open) {
			continue
		}
		end := strings.Index(text, tag.close)
		if end < 0 {
			return "", 0, false // unterminated: not a well-formed envelope
		}
		return tag.kind, end + len(tag.close), true
	}
	return "", 0, false
}

// harnessSurface returns a message's whole-text content when the message is
// client-authored scaffolding material: a plain-string `system`/`user`
// message that is exactly one or more well-formed envelopes. Tool-role output,
// the legacy `function` role, assistant turns, and multimodal content arrays
// are never scaffolding — an envelope smuggled through fetched tool output or
// inside an array stays ordinary untrusted text.
func harnessSurface(msg map[string]interface{}) (string, harnessEnvelopeKind, bool) {
	role, _ := msg["role"].(string)
	if role != "system" && role != "user" {
		return "", "", false
	}
	text, ok := msg["content"].(string)
	if !ok || text == "" {
		return "", "", false
	}
	kind, isHarness := classifyHarnessEnvelope(text)
	if !isHarness {
		return "", "", false
	}
	return text, kind, true
}

// wordingOnlyCategories reports whether every detection category on a surface
// is an imperative-wording category. Hidden-text, encoded, and custom-detector
// hits are carriers a harness envelope does not legitimately contain, so they
// keep the surface actionable.
func wordingOnlyCategories(categories map[injectionCategory]int) bool {
	for category := range categories {
		switch category {
		case categoryHiddenText, categoryEncoded, categoryCustom:
			return false
		}
	}
	return true
}
