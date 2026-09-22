package pipeline

import (
	"encoding/json"
	"strings"
)

// WalkMessageText applies fn to every text surface of a message: the
// content field (plain-string form and the text parts of multimodal
// content arrays) and assistant tool_calls[].function.arguments strings
// (a common secret carrier in command/env payloads). Non-text parts
// (image_url, input_audio, and other modalities) are left untouched;
// scope is OpenAI wire format — /v1/messages Anthropic requests are
// transformed before the chain runs, so nested tool_result blocks are
// covered transitively. An arguments replacement that breaks JSON
// validity is neutralized wholesale (the arguments object becomes a
// NENYA marker object) so the original text never survives. Returns
// true if any surface was modified.
func WalkMessageText(msg map[string]interface{}, fn func(string) string) bool {
	changed := walkMessageContent(msg, fn)
	if walkToolCallArguments(msg, fn) {
		changed = true
	}
	return changed
}

// newCountingRedactor wraps fn with label-delta counting, accumulating
// substitution counts into count. The delta can skew when a match engulfs
// pre-existing label occurrences, and (for tool-call argument surfaces)
// when the wholesale-neutralization marker replaces fn's output — both
// residual skews are accepted to keep counting allocation-free.
func newCountingRedactor(fn func(string) string, label string, count *int) func(string) string {
	return func(content string) string {
		redacted := fn(content)
		*count += strings.Count(redacted, label) - strings.Count(content, label)
		return redacted
	}
}

// walkMessageContent applies fn to the content field: the plain-string
// form and the text parts of multimodal content arrays. Returns true if
// any surface was modified.
func walkMessageContent(msg map[string]interface{}, fn func(string) string) bool {
	changed := false
	switch c := msg["content"].(type) {
	case string:
		if c == "" {
			return false
		}
		if r := fn(c); r != c {
			msg["content"] = r
			changed = true
		}
	case []interface{}:
		for _, partRaw := range c {
			part, ok := partRaw.(map[string]interface{})
			if !ok {
				continue
			}
			if part["type"] != "text" {
				continue
			}
			text, ok := part["text"].(string)
			if !ok || text == "" {
				continue
			}
			if r := fn(text); r != text {
				part["text"] = r
				changed = true
			}
		}
	}
	return changed
}

// walkToolCallArguments applies fn to the arguments string of each tool
// call on the message. Returns true if any arguments string was modified.
func walkToolCallArguments(msg map[string]interface{}, fn func(string) string) bool {
	calls, ok := msg["tool_calls"].([]interface{})
	if !ok {
		return false
	}
	changed := false
	for _, callRaw := range calls {
		call, ok := callRaw.(map[string]interface{})
		if !ok {
			continue
		}
		function, ok := call["function"].(map[string]interface{})
		if !ok {
			continue
		}
		args, ok := function["arguments"].(string)
		if !ok || args == "" {
			continue
		}
		r := fn(args)
		if r == args {
			continue
		}
		if !json.Valid([]byte(r)) {
			// A span replacement broke JSON validity: neutralize the
			// arguments wholesale with an action-neutral marker object
			// (fires for any interceptor whose rewrite breaks JSON) so
			// the original text never survives upstream.
			r = `{"nenya":"content_neutralized"}`
			if r == args {
				continue
			}
		}
		function["arguments"] = r
		changed = true
	}
	return changed
}
