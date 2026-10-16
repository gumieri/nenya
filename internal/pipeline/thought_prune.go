package pipeline

import (
	"strings"

	"github.com/nenya/config"
)

const (
	thoughtOpenTag  = "<think"
	thoughtCloseTag = "</think"
	thoughtMarker   = "[Reasoning pruned by gateway]"
)

// PruneThoughts removes <think ...>...</think > reasoning blocks from
// assistant message content to reduce token usage while preserving the final
// model output. A block whose close tag is missing (a stream-interrupted
// reasoning) is left INTACT (NENYA-150): its boundary cannot be verified, and
// dropping to end-of-string could swallow the answer that follows it.
//
// Mutations are confined to the last cfg.MutationWindow messages (default
// config.DefaultMutationWindow): assistant messages older than that are left
// byte-identical, so the stable prefix survives across turns and the provider
// prompt-cache prefix is not invalidated as the conversation grows. The window
// is measured against the message slice this function receives (it may already
// have been shortened by stale-tool pruning), independently of that pass.
//
// It does NOT remove the structured reasoning_content field from assistant
// messages — that is provider-specific data handled by
// routing.SanitizePayload per-target so that providers requiring it
// (e.g. DeepSeek v4 thinking mode) receive it intact.
func PruneThoughts(payload map[string]interface{}, cfg config.CompactionConfig) bool {
	if cfg.PruneThoughts == nil || !*cfg.PruneThoughts {
		return false
	}

	messagesRaw, ok := payload["messages"]
	if !ok {
		return false
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok || len(messages) == 0 {
		return false
	}

	mutated := false

	// Confine mutations to the tail: only the most recent MutationWindow
	// messages are eligible, so older assistant messages stay byte-identical
	// and the provider prompt-cache prefix survives as the conversation grows.
	start := pruneWindowStart(len(messages), cfg)

	for _, msgRaw := range messages[start:] {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}

		if pruneThoughtTags(msg) {
			mutated = true
		}
	}

	return mutated
}

// StripReasoningContent removes the reasoning_content field from all
// assistant messages in the payload. This should be called per-target
// in SanitizePayload for providers that do not support reasoning, to avoid
// sending unknown fields upstream.
func StripReasoningContent(payload map[string]interface{}) bool {
	messagesRaw, ok := payload["messages"]
	if !ok {
		return false
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok || len(messages) == 0 {
		return false
	}

	mutated := false

	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}

		rc, ok := msg["reasoning_content"]
		if !ok {
			continue
		}

		str, ok := rc.(string)
		if !ok {
			delete(msg, "reasoning_content")
			mutated = true
			continue
		}
		if str == "" {
			continue
		}

		delete(msg, "reasoning_content")
		mutated = true
	}

	return mutated
}

func pruneThoughtTags(msg map[string]interface{}) bool {
	content, ok := msg["content"]
	if !ok {
		return false
	}

	str, ok := content.(string)
	if !ok {
		return false
	}

	pruned := stripThoughtBlocks(str)
	if pruned == str {
		return false
	}

	msg["content"] = pruned
	return true
}

func stripThoughtBlocks(s string) string {
	if !hasPrunableThoughtBlock(s) {
		return s
	}

	var b strings.Builder

	rest := s
	for {
		idx := strings.Index(rest, thoughtOpenTag)
		if idx < 0 {
			if b.Len() == 0 {
				return s
			}
			b.WriteString(rest)
			break
		}

		if !isThinkTagBoundary(rest, idx+len(thoughtOpenTag)) {
			// False prefix (e.g. "<thinking!"): copy through it and keep
			// scanning — a real block may still follow (NENYA-150).
			b.WriteString(rest[:idx+len(thoughtOpenTag)])
			rest = rest[idx+len(thoughtOpenTag):]
			continue
		}

		if b.Len() == 0 {
			b.Grow(len(rest))
		}

		b.WriteString(rest[:idx])

		afterOpen := rest[idx+len(thoughtOpenTag):]

		closeIdx := findCloseTag(afterOpen)

		if closeIdx < 0 {
			// Unterminated block: the boundary cannot be verified, so the
			// remainder is kept INTACT (NENYA-150) — dropping to end-of-string
			// would swallow whatever follows the reasoning (possibly the
			// answer). Reasoning blocks are dropped only whole.
			b.WriteString(rest[idx:])
			break
		}

		b.WriteString(thoughtMarker)
		rest = afterOpen[closeIdx+closeTagLen(afterOpen[closeIdx:]):]
	}

	return b.String()
}

func isThinkTagBoundary(s string, pos int) bool {
	if pos >= len(s) {
		return true
	}
	ch := s[pos]
	return ch == '>' || ch == '\n' || ch == '\r' || ch == ' ' || ch == '\t'
}

// hasPrunableThoughtBlock cheaply pre-filters content with no
// boundary-valid block opener, so prose that merely mentions "<thinking"
// does not pay for a full rebuild (NENYA-150).
func hasPrunableThoughtBlock(s string) bool {
	for _, opener := range []string{thoughtOpenTag + ">", thoughtOpenTag + " ", thoughtOpenTag + "\t", thoughtOpenTag + "\n", thoughtOpenTag + "\r"} {
		if strings.Contains(s, opener) {
			return true
		}
	}
	return false
}

// closeTagLen returns the full length of the close tag at the start of s,
// including the terminating '>'. Whitespace between the tag and the '>' is
// part of the tag ("</think >"); whitespace AFTER the tag belongs to the
// following content and is never consumed (NENYA-150).
func closeTagLen(s string) int {
	n := len(thoughtCloseTag)
	j := n
	for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
		j++
	}
	if j < len(s) && s[j] == '>' {
		return j + 1
	}
	return n
}

func findCloseTag(s string) int {
	maxIter := len(s)
	for range maxIter {
		idx := strings.Index(s, thoughtCloseTag)
		if idx < 0 {
			return -1
		}
		if isThinkTagBoundary(s, idx+len(thoughtCloseTag)) {
			return idx
		}
		s = s[idx+len(thoughtCloseTag):]
	}
	return -1
}
