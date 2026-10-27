package adapter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"github.com/nenya/config"
	"github.com/nenya/internal/jsonschema"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/util"
)

func (a *AnthropicAdapter) convertOpenAIToAnthropic(openai map[string]interface{}, model string, stream bool, opts CacheOpts) map[string]interface{} {
	anthropic := map[string]interface{}{
		"model":      model,
		"stream":     stream,
		"max_tokens": 8192,
	}

	a.copyOpenAIFields(openai, anthropic)

	midConvoSystem := a.resolveMidConversationSystem(model, opts)
	hasMessages := false
	systemHoist := 0
	if msgs, ok := openai["messages"].([]interface{}); ok {
		hasMessages = ok
		if midConvoSystem {
			systemHoist = a.systemHoistCount(msgs)
		}
		anthropic["messages"] = a.convertMessages(msgs, systemHoist, midConvoSystem, opts.TrailingContextMarkers)
	}

	if tools, ok := openai["tools"].([]interface{}); ok && len(tools) > 0 {
		anthropic["tools"] = a.convertTools(tools, opts.Tools, opts.getToolsTTL())
	}

	if tc, ok := openai["tool_choice"]; ok {
		a.convertToolChoice(tc, anthropic)
	}

	a.injectReasoningEffort(openai, anthropic, model)

	if opts.Automatic {
		// Automatic caching lets Anthropic place and advance a single
		// breakpoint; no block-level markers are injected. The system
		// prompt must still be hoisted: returning before setSystemPrompt
		// silently dropped it (NENYA-171).
		if hasMessages {
			a.setSystemPrompt(openai, anthropic, systemPromptSpec{
				ttl: opts.getSystemTTL(), hoistCount: systemHoist,
				midConvoSystem: midConvoSystem, markers: opts.TrailingContextMarkers,
			})
		}
		anthropic["cache_control"] = buildCacheControl(opts.getSystemTTL())
		return anthropic
	}

	if hasMessages {
		a.setSystemPrompt(openai, anthropic, systemPromptSpec{
			cacheSystem: opts.System, ttl: opts.getSystemTTL(), hoistCount: systemHoist,
			midConvoSystem: midConvoSystem, markers: opts.TrailingContextMarkers,
		})
	}

	var claimedMarker map[string]interface{}
	if opts.Messages {
		claimedMarker = a.injectMessageCache(anthropic, opts.getMessagesTTL(), opts.TrailingContextMarkers)
	}
	capCacheMarkers(anthropic, claimedMarker)

	return anthropic
}

// resolveMidConversationSystem decides whether role=system messages may
// remain in the messages array. An explicit CacheOpts override wins;
// otherwise the model family decides via discovery inference.
func (a *AnthropicAdapter) resolveMidConversationSystem(model string, opts CacheOpts) bool {
	if opts.MidConversationSystem != nil {
		return *opts.MidConversationSystem
	}
	return util.SupportsMidConversationSystem(model)
}

// systemHoistCount returns how many leading messages are hoisted into the
// top-level system prompt. Only meaningful for mid-conversation-capable
// models, which hoist the contiguous system run at the head and keep
// later system reminders in place with their cache_control breakpoints
// intact. Legacy models ignore the count and hoist every system message.
func (a *AnthropicAdapter) systemHoistCount(msgs []interface{}) int {
	for i, msgRaw := range msgs {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			return i
		}
		if role, _ := msg["role"].(string); role != "system" {
			return i
		}
	}
	return len(msgs)
}

// systemPromptSpec groups setSystemPrompt's placement parameters (AUD-81 /
// NENYA-316: seven positional parameters beyond the options threshold).
type systemPromptSpec struct {
	// cacheSystem selects the cache_control-anchored system block form.
	cacheSystem bool
	// ttl is the cache TTL spelling for the anchored block.
	ttl string
	// hoistCount bounds the mid-conversation hoist prefix.
	hoistCount int
	// midConvoSystem enables the mid-conversation hoist (NENYA-171).
	midConvoSystem bool
	// markers are the trailing-context markers excluded from the hoist.
	markers []string
}

func (a *AnthropicAdapter) setSystemPrompt(openai, anthropic map[string]interface{}, spec systemPromptSpec) {
	cacheSystem, ttl := spec.cacheSystem, spec.ttl
	hoistCount, midConvoSystem, markers := spec.hoistCount, spec.midConvoSystem, spec.markers
	msgs, ok := openai["messages"].([]interface{})
	if !ok {
		return
	}
	if midConvoSystem {
		if hoistCount > len(msgs) {
			hoistCount = len(msgs)
		}
		msgs = msgs[:hoistCount]
	}
	systemParts := a.extractSystemMessages(msgs, markers)
	if len(systemParts) == 0 {
		return
	}
	joined := strings.Join(systemParts, "\n\n")
	if cacheSystem {
		anthropic["system"] = []interface{}{
			map[string]interface{}{
				"type":          "text",
				"text":          joined,
				"cache_control": buildCacheControl(ttl),
			},
		}
	} else {
		anthropic["system"] = joined
	}
}

func (a *AnthropicAdapter) injectReasoningEffort(openai, anthropic map[string]interface{}, model string) {
	reasoningEffort, ok := openai["reasoning_effort"].(string)
	if !ok || reasoningEffort == "" {
		return
	}
	if _, hasThinking := anthropic["thinking"]; hasThinking {
		return
	}
	if model == "" {
		return
	}

	lowerModel := strings.ToLower(model)

	entry, ok := config.ModelRegistry[lowerModel]
	if !ok || entry.Thinking.Max <= 0 {
		return
	}

	budget := a.budgetForEffort(reasoningEffort, entry.Thinking.Min, entry.Thinking.Max)
	if maxTokens, ok := anthropic["max_tokens"].(int); ok && maxTokens > 0 && budget > maxTokens {
		budget = maxTokens
	}

	thinkingObj := map[string]interface{}{
		"type":          thinkingTypeEnabled,
		"budget_tokens": budget,
	}
	if entry.Thinking.Adaptive {
		// Adaptive models (Opus 4.7+, Sonnet 5, Fable 5) support display override.
		// NOTE: The exact wire format for "adaptive thinking" needs verification against live API.
		// The AI SDK uses {thinking: { type: "adaptive", display: "summarized" }} with a sibling `effort` key.
		// For now, we use the proven format: type:"enabled" with budget_tokens + display:"summarized".
		thinkingObj["display"] = thinkingDisplaySummarized
	}

	anthropic["thinking"] = thinkingObj
}

func (a *AnthropicAdapter) budgetForEffort(effort string, min, max int) int {
	var budget int
	switch strings.ToLower(effort) {
	case "low", "minimal":
		budget = min
	case "medium":
		budget = max / 4
	case "high":
		budget = max / 2
	case "xhigh":
		budget = max - max/4
	case "max":
		budget = max
	default:
		budget = max / 4
	}
	if budget < min {
		budget = min
	}
	if budget > max {
		budget = max
	}
	return budget
}

// injectMessageCache claims the gateway's message-level breakpoint and
// returns the block it claimed (nil when it left the position alone — a
// client marker is the client's). The claimed block is a COPY: the
// converted body may alias the caller's payload (reused across failover
// targets), and an Anthropic-only marker must not leak into later
// OpenAI-format bodies (NENYA-171).
func (a *AnthropicAdapter) injectMessageCache(anthropic map[string]interface{}, ttl string, markers []string) map[string]interface{} {
	msgs, ok := anthropic["messages"].([]interface{})
	if !ok || len(msgs) < 2 {
		return nil
	}

	// NENYA-207: the anchor must keep the cached prefix CLIENT-STABLE. The
	// previous rule (last-but-one message) assumed the tail is the in-flight
	// turn; on MCP loop iterations the array continues PAST the request-
	// varying canary trailer with gateway-produced tool traffic, so the
	// marker could land on a gateway message adjacent to the random canary
	// token — anchoring a prefix that differs on every later request and
	// missing the cache. When a trailer marker is present, anchor the last
	// message AHEAD of it instead — including when the trailer is the first
	// message (only client forgery can produce that shape): there is no
	// client-stable history ahead of it, so no anchor is claimed.
	end := len(msgs) - 2
	if idx := a.firstTrailingMarkerIndex(msgs, markers); idx >= 0 {
		end = idx - 1
	}
	if end < 0 {
		return nil
	}

	targetMsg, ok := msgs[end].(map[string]interface{})
	if !ok {
		return nil
	}

	content, ok := targetMsg["content"].([]interface{})
	if !ok || len(content) == 0 {
		return nil
	}

	lastBlock, ok := content[len(content)-1].(map[string]interface{})
	if !ok {
		return nil
	}
	if _, clientMarked := lastBlock["cache_control"]; clientMarked {
		// The client stated its own breakpoint (possibly 1h): keep it
		// instead of downgrading to the configured default.
		return nil
	}
	claimed := make(map[string]interface{}, util.AddCap(len(lastBlock), 1))
	for k, v := range lastBlock {
		claimed[k] = v
	}
	claimed["cache_control"] = buildCacheControl(ttl)
	content[len(content)-1] = claimed
	return claimed
}

// firstTrailingMarkerIndex returns the index of the first message matching a
// trailing-context marker (the gateway's request-varying canary trailer), or
// -1 when none is present. The converted body carries the trailer as content
// BLOCKS (convertMessages emits it as a trailing user message), so both wire
// shapes are matched.
func (a *AnthropicAdapter) firstTrailingMarkerIndex(msgs []interface{}, markers []string) int {
	if len(markers) == 0 {
		return -1
	}
	for i, raw := range msgs {
		msg, isMap := raw.(map[string]interface{})
		if !isMap {
			continue
		}
		if text, isStr := msg["content"].(string); isStr {
			if matchesTrailingMarker(text, markers) {
				return i
			}
			continue
		}
		blocks, ok := msg["content"].([]interface{})
		if !ok {
			continue
		}
		for _, b := range blocks {
			block, ok := b.(map[string]interface{})
			if !ok {
				continue
			}
			if text, ok := block["text"].(string); ok && matchesTrailingMarker(text, markers) {
				return i
			}
		}
	}
	return -1
}

// capCacheMarkers enforces Anthropic's four-breakpoint limit: when
// passthrough client markers plus the gateway's own exceed the budget, the
// gateway's markers are dropped first (message, then tools, then system) —
// a client's explicit placement wins over the gateway's default, so the
// API never sees a request it would reject with 400 (NENYA-171).
// claimedMarker is the exact block injectMessageCache claimed (nil when the
// position holds a client marker, which is never touched).
func capCacheMarkers(anthropic map[string]interface{}, claimedMarker map[string]interface{}) {
	if countCacheMarkers(anthropic) <= maxAnthropicCacheMarkers {
		return
	}
	if claimedMarker != nil {
		delete(claimedMarker, "cache_control")
	}
	if countCacheMarkers(anthropic) <= maxAnthropicCacheMarkers {
		return
	}
	if tools, ok := anthropic["tools"].([]interface{}); ok && len(tools) > 0 {
		if last, ok := tools[len(tools)-1].(map[string]interface{}); ok {
			delete(last, "cache_control")
		}
	}
	if countCacheMarkers(anthropic) <= maxAnthropicCacheMarkers {
		return
	}
	if system, ok := anthropic["system"].([]interface{}); ok && len(system) > 0 {
		if last, ok := system[len(system)-1].(map[string]interface{}); ok {
			delete(last, "cache_control")
		}
	}
}

// maxAnthropicCacheMarkers is the API's per-request breakpoint limit.
const maxAnthropicCacheMarkers = 4

// countCacheMarkers counts cache_control markers at the positions the
// Anthropic API accepts (system blocks, the last tool block, message
// content blocks) — a tool input_schema property or a tool_use argument
// that happens to be named cache_control is not a breakpoint (NENYA-171).
func countCacheMarkers(anthropic map[string]interface{}) int {
	n := 0
	if system, ok := anthropic["system"].([]interface{}); ok {
		n += countBlockMarkers(system)
	}
	if tools, ok := anthropic["tools"].([]interface{}); ok && len(tools) > 0 {
		if last, ok := tools[len(tools)-1].(map[string]interface{}); ok {
			if _, marked := last["cache_control"]; marked {
				n++
			}
		}
	}
	if msgs, ok := anthropic["messages"].([]interface{}); ok {
		for _, raw := range msgs {
			msg, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if content, ok := msg["content"].([]interface{}); ok {
				n += countBlockMarkers(content)
			}
		}
	}
	return n
}

// countBlockMarkers counts cache_control keys among content blocks.
func countBlockMarkers(blocks []interface{}) int {
	n := 0
	for _, raw := range blocks {
		if block, ok := raw.(map[string]interface{}); ok {
			if _, marked := block["cache_control"]; marked {
				n++
			}
		}
	}
	return n
}

// firstTokenCap returns the payload's token cap under either spelling:
// max_tokens is the canonical field, max_completion_tokens the newer one a
// target-aware normalizer may have moved the cap to (NENYA-179). Reading only
// max_tokens silently discarded the cap (and the model clamp) for
// Anthropic-format targets.
func firstTokenCap(openai map[string]interface{}) interface{} {
	// A non-positive cap is not usable: skip it so the other spelling (or the
	// caller's default) wins instead of suppressing the fallback
	// (NENYA-179 review).
	if v, ok := numericTokenValue(openai["max_tokens"]); ok && v > 0 {
		return v
	}
	if v, ok := numericTokenValue(openai["max_completion_tokens"]); ok && v > 0 {
		return v
	}
	return nil
}

// numericTokenValue coerces a token-count field to float64. The gateway
// injects/clamps max_tokens in memory (int), while a decoded client body
// carries float64 — asserting only float64 silently dropped the model output
// cap for Anthropic-format targets, leaving the hardcoded default to win
// (NENYA-186 review).
func numericTokenValue(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

func (a *AnthropicAdapter) copyOpenAIFields(openai, anthropic map[string]interface{}) {
	if v, ok := numericTokenValue(firstTokenCap(openai)); ok && v > 0 {
		if v > math.MaxInt32 {
			v = math.MaxInt32
		}
		anthropic["max_tokens"] = int(v)
	}
	if v, ok := openai["temperature"].(float64); ok {
		anthropic["temperature"] = v
	}
	if v, ok := openai["top_p"].(float64); ok {
		anthropic["top_p"] = v
	}
	if v, ok := openai["stop"]; ok {
		anthropic["stop_sequences"] = v
	}
	if v, ok := openai["user"].(string); ok {
		anthropic["metadata"] = map[string]interface{}{
			"user_id": v,
		}
	}
	if v, ok := openai["stream"].(bool); ok {
		anthropic["stream"] = v
	}
	// A client-set thinking config is a request knob (NENYA-151): carry it
	// when it is Anthropic-shaped. Copied, not aliased: the payload is
	// reused across failover targets and must not gain Anthropic-only
	// fields.
	if v, ok := openai["thinking"].(map[string]interface{}); ok {
		if shaped := anthropicShapedThinking(v); shaped != nil {
			anthropic["thinking"] = shaped
		}
	}
}

// anthropicShapedThinking validates a client thinking map against the
// Anthropic schema (type required; budget_tokens required when enabled) and
// returns a copy holding only Anthropic-known keys. A map from another
// dialect — e.g. the zai-shaped {type, clear_thinking} the sanitize hooks
// inject into the shared failover payload — returns nil, restoring the
// drop: carrying it verbatim would produce a guaranteed-400 body on
// Anthropic-format targets (NENYA-151).
func anthropicShapedThinking(v map[string]interface{}) map[string]interface{} {
	typ, has := v["type"].(string)
	if !has || typ == "" {
		return nil
	}
	copied := map[string]interface{}{"type": typ}
	if typ != "enabled" {
		return copied
	}
	budget, has := v["budget_tokens"]
	if !has {
		return nil
	}
	copied["budget_tokens"] = budget
	return copied
}

func (a *AnthropicAdapter) extractSystemMessages(msgs []interface{}, markers []string) []string {
	var systemParts []string
	for _, msgRaw := range msgs {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		if isTrailingContextMarker(msg, markers) {
			continue
		}
		if role, _ := msg["role"].(string); role == "system" {
			systemParts = append(systemParts, a.extractSystemContent(msg["content"])...)
		}
	}
	return systemParts
}

// isTrailingContextMarker reports whether a message carries a
// request-varying gateway trailer (the egress canary marker): those are
// excluded from the cached system block and kept at the tail (NENYA-171).
func isTrailingContextMarker(msg map[string]interface{}, markers []string) bool {
	if len(markers) == 0 {
		return false
	}
	text, ok := msg["content"].(string)
	if !ok {
		return false
	}
	return matchesTrailingMarker(text, markers)
}

// matchesTrailingMarker reports whether text carries the gateway's marker
// SHAPE (prefix plus the internal-ref frame), not a bare substring a client
// might merely mention (NENYA-171).
func matchesTrailingMarker(text string, markers []string) bool {
	return pipeline.MatchesTrailingMarker(text, markers)
}

func (a *AnthropicAdapter) extractSystemContent(content interface{}) []string {
	var parts []string
	switch c := content.(type) {
	case string:
		if c != "" {
			parts = append(parts, c)
		}
	case []interface{}:
		for _, partRaw := range c {
			if part, ok := partRaw.(map[string]interface{}); ok {
				if t, ok := part["text"].(string); ok && t != "" {
					parts = append(parts, t)
				}
			}
		}
	}
	return parts
}

func (a *AnthropicAdapter) convertToolChoice(tc interface{}, anthropic map[string]interface{}) {
	if s, ok := tc.(string); ok {
		switch s {
		case "auto", "required":
			anthropic["tool_choice"] = map[string]interface{}{"type": s}
		case "none":
			delete(anthropic, "tool_choice")
		}
		return
	}
	if m, ok := tc.(map[string]interface{}); ok {
		if fn, ok := m["function"].(map[string]interface{}); ok {
			if name, ok := fn["name"].(string); ok {
				anthropic["tool_choice"] = map[string]interface{}{
					"type": "tool",
					"name": name,
				}
			}
		}
	}
}

func (a *AnthropicAdapter) convertMessages(msgs []interface{}, systemHoist int, midConvoSystem bool, markers []string) []interface{} {
	result := make([]interface{}, 0, len(msgs))
	var lastAssistantToolIDs map[string]bool

	for i := 0; i < len(msgs); i++ {
		msg, ok := msgs[i].(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := msg["role"].(string)

		if role == "system" {
			if (i < systemHoist || !midConvoSystem) && !isTrailingContextMarker(msg, markers) {
				// Hoisted into the top-level system prompt (legacy
				// models hoist every system message regardless of
				// position; capable models hoist only the head run). A
				// request-varying gateway trailer is never hoisted: it
				// stays at the tail, unmarked (NENYA-171).
				continue
			}
			if isTrailingContextMarker(msg, markers) && !midConvoSystem {
				// Legacy hoisting models do not accept a system role
				// inside messages; the request-varying trailer is emitted
				// as a trailing USER message with flat text so the
				// tripwire still reaches the upstream (NENYA-171).
				text := strings.Join(a.extractSystemContent(msg["content"]), "\n\n")
				result = append(result, map[string]interface{}{
					"role": "user",
					"content": []interface{}{map[string]interface{}{
						"type": "text",
						"text": text,
					}},
				})
				continue
			}
			// Mid-conversation system reminder: keep in place with
			// verbatim content blocks so client cache_control
			// breakpoints survive the conversion.
			result = append(result, map[string]interface{}{
				"role":    "system",
				"content": a.buildContentBlocks(msg),
			})
			continue
		}

		if role == "tool" {
			toolBatch := a.collectToolMessages(msgs, &i)
			toolResults := a.buildValidatedToolResults(toolBatch, lastAssistantToolIDs)
			lastAssistantToolIDs = nil
			if len(toolResults) == 0 {
				continue
			}
			anthMsg := map[string]interface{}{
				"role":    "user",
				"content": toolResults,
			}
			result = append(result, anthMsg)
			continue
		}

		contentBlocks := a.buildContentBlocks(msg)
		anthMsg := map[string]interface{}{
			"role":    role,
			"content": contentBlocks,
		}
		result = append(result, anthMsg)

		if role == "assistant" {
			ids := a.extractToolUseIDsFromBlocks(contentBlocks)
			if len(ids) > 0 {
				lastAssistantToolIDs = ids
			} else {
				lastAssistantToolIDs = nil
			}
		} else {
			lastAssistantToolIDs = nil
		}
	}
	return a.sanitizeOrphanedToolUse(result)
}

// sanitizeOrphanedToolUse strips tool_use blocks from assistant messages
// when the immediately following user message lacks matching tool_result blocks.
// Anthropic's API requires every tool_use to have a corresponding tool_result
// in the next message. This handles cases where message pruning or other
// preprocessing has broken the pairing.
func (a *AnthropicAdapter) sanitizeOrphanedToolUse(result []interface{}) []interface{} {
	if len(result) < 2 {
		return result
	}

	for i := 0; i < len(result); i++ {
		msg, ok := result[i].(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}

		content, ok := msg["content"].([]interface{})
		if !ok || len(content) == 0 {
			continue
		}

		toolUseIDs := a.collectToolUseIDs(content)
		if len(toolUseIDs) == 0 {
			continue
		}

		nextResultIDs := a.collectNextToolResultIDs(result, i)
		orphaned := a.findOrphanedIDs(toolUseIDs, nextResultIDs)
		if len(orphaned) == 0 {
			continue
		}

		for _, id := range orphaned {
			slog.Warn("anthropic: stripping orphaned tool_use block (no matching tool_result)", "tool_use_id", id)
		}

		result[i] = a.stripToolUseBlocks(msg, content, orphaned)
	}

	return result
}

// collectToolUseIDs returns the tool_use IDs present in the given content blocks.
func (a *AnthropicAdapter) collectToolUseIDs(blocks []interface{}) []string {
	var ids []string
	for _, block := range blocks {
		bm, ok := block.(map[string]interface{})
		if !ok || bm["type"] != "tool_use" {
			continue
		}
		if id, _ := bm["id"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// collectNextToolResultIDs returns the set of tool_use_id values in the message
// immediately following result[i]. Returns nil if there is no next message or
// the next message is not a user message with tool_result content.
func (a *AnthropicAdapter) collectNextToolResultIDs(result []interface{}, i int) map[string]bool {
	if i+1 >= len(result) {
		return nil
	}
	nextMsg, ok := result[i+1].(map[string]interface{})
	if !ok {
		return nil
	}
	if role, _ := nextMsg["role"].(string); role != "user" {
		return nil
	}
	nextContent, ok := nextMsg["content"].([]interface{})
	if !ok {
		return nil
	}
	ids := make(map[string]bool, len(nextContent))
	for _, block := range nextContent {
		bm, ok := block.(map[string]interface{})
		if !ok || bm["type"] != "tool_result" {
			continue
		}
		if id, _ := bm["tool_use_id"].(string); id != "" {
			ids[id] = true
		}
	}
	return ids
}

// findOrphanedIDs returns tool_use IDs that are not present in nextResultIDs.
// Returns nil when no IDs are orphaned.
func (a *AnthropicAdapter) findOrphanedIDs(toolUseIDs []string, nextResultIDs map[string]bool) []string {
	if nextResultIDs == nil {
		// No next message at all — all tool_use IDs are orphaned
		result := make([]string, len(toolUseIDs))
		copy(result, toolUseIDs)
		return result
	}
	var orphaned []string
	for _, id := range toolUseIDs {
		if !nextResultIDs[id] {
			orphaned = append(orphaned, id)
		}
	}
	return orphaned
}

// stripToolUseBlocks removes orphaned tool_use content blocks from the message
// and returns a new map. If all blocks are removed, a fallback text block is added.
func (a *AnthropicAdapter) stripToolUseBlocks(msg map[string]interface{}, content []interface{}, orphaned []string) map[string]interface{} {
	orphanedSet := make(map[string]bool, len(orphaned))
	for _, id := range orphaned {
		orphanedSet[id] = true
	}

	newContent := make([]interface{}, 0, len(content))
	for _, block := range content {
		bm, ok := block.(map[string]interface{})
		if !ok {
			newContent = append(newContent, block)
			continue
		}
		if bm["type"] == "tool_use" {
			if id, _ := bm["id"].(string); orphanedSet[id] {
				continue
			}
		}
		newContent = append(newContent, block)
	}

	if len(newContent) == 0 {
		newContent = []interface{}{emptyTextBlock}
	}

	updated := make(map[string]interface{}, len(msg))
	for k, v := range msg {
		updated[k] = v
	}
	updated["content"] = newContent
	return updated
}

// collectToolMessages gathers consecutive tool-role messages starting at index *i.
// It advances *i past all tool messages consumed. If it encounters a non-tool
// message, it decrements *i so the caller's i++ lands on it. Returns the batch
// of raw tool messages for validation.
func (a *AnthropicAdapter) collectToolMessages(msgs []interface{}, i *int) []map[string]interface{} {
	var batch []map[string]interface{}
	for *i < len(msgs) {
		msg, ok := msgs[*i].(map[string]interface{})
		if !ok {
			break
		}
		role, _ := msg["role"].(string)
		if role != "tool" {
			(*i)--
			break
		}
		batch = append(batch, msg)
		(*i)++
	}
	return batch
}

// extractToolUseIDsFromBlocks scans Anthropic content blocks and returns a set
// of all tool_use block IDs. Returns a non-nil map only when tool_use blocks exist.
func (a *AnthropicAdapter) extractToolUseIDsFromBlocks(blocks []interface{}) map[string]bool {
	ids := make(map[string]bool)
	for _, block := range blocks {
		bm, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		if typ, _ := bm["type"].(string); typ == "tool_use" {
			if id, ok := bm["id"].(string); ok && id != "" {
				ids[id] = true
			}
		}
	}
	return ids
}

// buildValidatedToolResults validates each tool message's tool_call_id against the
// set of known tool_use IDs from the preceding assistant message. Orphaned results
// (nil validIDs, missing tool_call_id, or ID not in validIDs) are dropped with a warning.
func (a *AnthropicAdapter) buildValidatedToolResults(batch []map[string]interface{}, validIDs map[string]bool) []interface{} {
	var results []interface{}
	for _, msg := range batch {
		toolCallID, _ := msg["tool_call_id"].(string)
		if toolCallID == "" {
			slog.Warn("anthropic: dropping tool message without tool_call_id")
			continue
		}
		if validIDs == nil {
			slog.Warn("anthropic: dropping orphaned tool result (no preceding assistant)", "tool_use_id", toolCallID)
			continue
		}
		if !validIDs[toolCallID] {
			slog.Warn("anthropic: dropping orphaned tool result (no matching tool_use)", "tool_use_id", toolCallID)
			continue
		}
		results = append(results, a.buildToolResultBlock(msg["content"], toolCallID))
	}
	return results
}

// buildToolResultBlock constructs a single Anthropic tool_result content block
// from a tool message's content and the validated tool_use_id.
func (a *AnthropicAdapter) buildToolResultBlock(content interface{}, toolUseID string) map[string]interface{} {
	toolResult := map[string]interface{}{
		"type":        "tool_result",
		"tool_use_id": toolUseID,
	}

	if content == nil {
		toolResult["content"] = ""
	} else {
		switch c := content.(type) {
		case string:
			toolResult["content"] = c
		case []interface{}:
			anthropicBlocks := a.convertToolContentBlocks(c)
			if len(anthropicBlocks) > 0 {
				toolResult["content"] = anthropicBlocks
			} else {
				toolResult["content"] = ""
			}
		default:
			slog.Debug("anthropic: tool message content has unexpected type, treating as string", "type", fmt.Sprintf("%T", content))
			toolResult["content"] = fmt.Sprintf("%v", content)
		}
	}

	return toolResult
}

// emptyTextBlock is a minimal Anthropic text content block used as a fallback
// when a message has no text or tool_use content after sanitization. Using a minimal
// placeholder satisfies Anthropic's API requirement that assistant messages have
// non-empty content. The "..." placeholder indicates content was removed.
var emptyTextBlock = map[string]interface{}{"type": "text", "text": "..."}

// buildContentBlocks constructs an Anthropic content array from an OpenAI-format
// message. It converts text content into typed text blocks and OpenAI tool_calls
// into Anthropic tool_use blocks. Returns a non-empty slice guaranteed to have at
// least one content block to satisfy Anthropic's API requirement.
func (a *AnthropicAdapter) buildContentBlocks(msg map[string]interface{}) []interface{} {
	blocks := make([]interface{}, 0, 2)

	content := msg["content"]
	switch c := content.(type) {
	case string:
		if strings.TrimSpace(c) != "" {
			blocks = append(blocks, map[string]interface{}{
				"type": "text",
				"text": c,
			})
		}
	case []interface{}:
		for _, part := range c {
			if pm, ok := part.(map[string]interface{}); ok {
				blocks = append(blocks, pm)
			}
		}
	}

	if role, _ := msg["role"].(string); role == "assistant" {
		if tcs, ok := msg["tool_calls"].([]interface{}); ok {
			for _, tc := range tcs {
				toolUse := a.convertToolCallToUse(tc)
				if toolUse != nil {
					blocks = append(blocks, toolUse)
				}
			}
		}
	}

	if len(blocks) == 0 {
		blocks = []interface{}{emptyTextBlock}
	}
	return blocks
}

// convertToolCallToUse converts a single OpenAI tool_call object to an Anthropic
// tool_use content block. Returns nil if the input is not a valid function tool call.
func (a *AnthropicAdapter) convertToolCallToUse(tc interface{}) map[string]interface{} {
	tcm, ok := tc.(map[string]interface{})
	if !ok {
		return nil
	}
	fn, ok := tcm["function"].(map[string]interface{})
	if !ok {
		return nil
	}

	id, _ := tcm["id"].(string)
	name, _ := fn["name"].(string)

	var input interface{}
	if args, ok := fn["arguments"].(string); ok && args != "" {
		_ = json.Unmarshal([]byte(args), &input)
	}
	if input == nil {
		input = map[string]interface{}{}
	}

	return map[string]interface{}{
		"type":  "tool_use",
		"id":    id,
		"name":  name,
		"input": input,
	}
}

func (a *AnthropicAdapter) convertToolContentBlocks(blocks []interface{}) []interface{} {
	var result []interface{}
	for _, blockRaw := range blocks {
		block, ok := blockRaw.(map[string]interface{})
		if !ok {
			continue
		}

		blockType, _ := block["type"].(string)
		switch blockType {
		case "text":
			if text, ok := block["text"].(string); ok {
				result = append(result, map[string]interface{}{
					"type": "text",
					"text": text,
				})
			}
		case "image_url":
			if imageURL, ok := block["image_url"].(map[string]interface{}); ok {
				if url, ok := imageURL["url"].(string); ok {
					parts := strings.SplitN(url, ",", 2)
					if len(parts) == 2 && strings.HasPrefix(parts[0], "data:") {
						mediaType := strings.TrimPrefix(parts[0], "data:")
						mediaType = strings.TrimSuffix(mediaType, ";base64")
						result = append(result, map[string]interface{}{
							"type": "image",
							"source": map[string]interface{}{
								"type":       "base64",
								"media_type": mediaType,
								"data":       parts[1],
							},
						})
					}
				}
			}
		case "image":
			if source, ok := block["source"].(map[string]interface{}); ok {
				result = append(result, map[string]interface{}{
					"type":   "image",
					"source": source,
				})
			}
		case "tool_result":
			slog.Debug("anthropic: tool_result block inside tool message content, forwarding as-is")
			result = append(result, block)
		}
	}
	return result
}

func (a *AnthropicAdapter) convertTools(tools []interface{}, cacheTools bool, ttl string) []interface{} {
	var result []interface{}
	for _, toolRaw := range tools {
		tool, ok := toolRaw.(map[string]interface{})
		if !ok {
			continue
		}

		if tool["type"] != "function" {
			continue
		}

		fn, ok := tool["function"].(map[string]interface{})
		if !ok {
			continue
		}

		name, _ := fn["name"].(string)
		desc, _ := fn["description"].(string)

		anthTool := map[string]interface{}{
			"name":        name,
			"description": desc,
			// Sanitized for cross-provider transport (NENYA-172): a
			// parameterless tool gets the minimal object schema and dialect
			// keywords, boolean subschemas, unresolved $refs, unsupported
			// regexes, and NUL escapes are removed.
			"input_schema": jsonschema.Sanitize(fn["parameters"]),
		}

		result = append(result, anthTool)
	}

	if cacheTools && len(result) > 0 {
		lastTool, ok := result[len(result)-1].(map[string]interface{})
		if ok {
			lastTool["cache_control"] = buildCacheControl(ttl)
		}
	}

	return result
}
