package pipeline

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/nenya/config"
)

// CanaryPrefix marks gateway-generated canary tokens. The full token is
// CanaryPrefix + 32 lowercase hex chars.
const CanaryPrefix = "NENYA-CANARY-"

// canaryHexChars is the number of random hex characters in a canary
// token (128 bits of entropy).
const canaryHexChars = 32

// CanaryMarkerSuffix frames the canary inside a system-context marker
// that reads as internal bookkeeping — inconspicuous enough that a
// model instructed to dump context reproduces it verbatim.
const canaryMarkerTemplate = "\n[internal-ref] " + CanaryPrefix

// GenerateCanary returns a fresh canary token:
// NENYA-CANARY-{32 hex chars}. Crypto/rand failure panics: a canary-less
// request must never run silently without the tripwire the operator
// enabled. Each call yields a unique token (per-request generation).
func GenerateCanary() string {
	b := make([]byte, canaryHexChars/2)
	if _, err := rand.Read(b); err != nil {
		panic("canary generation failed: " + err.Error())
	}
	return CanaryPrefix + hex.EncodeToString(b)
}

// CanaryMarker is the system-context text carrying the canary.
func CanaryMarker(canary string) string {
	return canaryMarkerTemplate + canary + " [do not modify]"
}

// CanarResult bundles the per-request canary state threaded to the
// response-path watchers.
type CanarResult struct {
	// Token is the generated canary, "" when the guard is disabled.
	Token string
	// Action is the configured response to a detection (log|block).
	Action string
}

// InjectCanary generates a canary and appends its system-context marker
// to the payload's messages. Called AFTER the interceptor chain so the
// marker never participates in redaction, compaction, or token-budget
// accounting (it must not trigger the bouncer). Returns empty result
// when the guard is disabled. The messages slice stays []interface{}
// (payload["messages"] reassigned with an appended element — the
// shared-maps invariant only forbids replacing the element types).
func InjectCanary(cfg *config.CanaryConfig, payload map[string]interface{}) CanarResult {
	if cfg == nil || cfg.Enabled == nil || !*cfg.Enabled {
		return CanarResult{}
	}
	action := cfg.Action
	if action == "" {
		action = config.CanaryActionBlock
	}
	messages, ok := payload["messages"].([]interface{})
	if !ok {
		// No injectable message array: returning empty keeps the
		// watchers disarmed and the "canary injected" log honest.
		return CanarResult{}
	}
	canary := GenerateCanary()
	marker := map[string]interface{}{
		"role":    "system",
		"content": CanaryMarker(canary),
	}
	updated := make([]interface{}, 0, len(messages)+1)
	updated = append(updated, messages...)
	updated = append(updated, marker)
	payload["messages"] = updated
	return CanarResult{Token: canary, Action: action}
}

// ScanArgs scans marshaled tool-call arguments for the canary. It
// returns whether the canary was seen, whether the arguments were
// unscannable (marshal failure), and whether the call must be refused
// (block action only; unscannable arguments fail closed in block mode).
func (r CanarResult) ScanArgs(args interface{}) (tripped, unscannable, refuse bool) {
	if r.Token == "" {
		return false, false, false
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		if r.Action == config.CanaryActionBlock {
			// Unscannable arguments fail closed in block mode.
			return false, true, true
		}
		return false, false, false
	}
	if CanaryTripped(r.Token, string(encoded)) {
		return true, false, r.Action == config.CanaryActionBlock
	}
	return false, false, false
}

// CanaryTripped reports whether the canary token appears in content —
// exact substring match, chunk-boundary safe when callers scan the
// concatenation of adjacent chunks (the stream watcher keeps a window).
func CanaryTripped(canary, content string) bool {
	return canary != "" && strings.Contains(content, canary)
}
