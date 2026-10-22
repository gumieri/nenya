package adapter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"

	"github.com/nenya/config"
	"github.com/nenya/internal/jsonschema"
	"github.com/nenya/internal/stream"
	"github.com/nenya/internal/util"
)

const (
	maxPreFlightBodyBytes     = 1 * 1024 * 1024
	thinkingTypeEnabled       = "enabled"
	thinkingDisplaySummarized = "summarized"
)

// CacheOpts groups cache control options for Anthropic adapter methods.
type CacheOpts struct {
	System      bool   // Enable system breakpoint caching
	Tools       bool   // Enable tools breakpoint caching
	Messages    bool   // Enable messages breakpoint caching
	Automatic   bool   // Enable top-level automatic caching mode (Anthropic automatic caching). When true, a single top-level cache_control is emitted instead of block-level markers.
	SystemTTL   string // TTL for system breakpoint ("ephemeral" or "1h")
	ToolsTTL    string // TTL for tools breakpoint
	MessagesTTL string // TTL for messages breakpoint
	GlobalTTL   string // Fallback TTL for all breakpoints (from CacheControlTTL)
	// MidConversationSystem overrides the model-capability probe for
	// keeping role=system messages in the messages array (Anthropic
	// 4.8+/5-family). nil lets the adapter decide via
	// discovery.SupportsMidConversationSystem; a non-nil value wins so
	// catalog/config-driven metadata can steer legacy hoisting either
	// way.
	MidConversationSystem *bool
	// TrailingContextMarkers lists content substrings identifying
	// gateway-injected, REQUEST-VARYING trailer messages (the egress
	// canary marker): they are never hoisted into the cache-marked system
	// block (that would change the cached prefix every request and miss
	// every breakpoint) and stay at the tail, unmarked, so the tripwire
	// still reaches the upstream (NENYA-171).
	TrailingContextMarkers []string
}

// getSystemTTL returns the effective TTL for the system breakpoint.
func (o *CacheOpts) getSystemTTL() string {
	if o.SystemTTL != "" {
		return o.SystemTTL
	}
	return o.GlobalTTL
}

// getToolsTTL returns the effective TTL for the tools breakpoint.
func (o *CacheOpts) getToolsTTL() string {
	if o.ToolsTTL != "" {
		return o.ToolsTTL
	}
	return o.GlobalTTL
}

// getMessagesTTL returns the effective TTL for the messages breakpoint.
func (o *CacheOpts) getMessagesTTL() string {
	if o.MessagesTTL != "" {
		return o.MessagesTTL
	}
	return o.GlobalTTL
}

// buildCacheControl constructs the cache_control map for Anthropic API.
// Returns {"type": "ephemeral"} for ephemeral TTL, or {"type": "ephemeral", "ttl": "1h"} for 1h TTL.
func buildCacheControl(ttl string) map[string]interface{} {
	cc := map[string]interface{}{"type": "ephemeral"}
	if ttl == "1h" {
		cc["ttl"] = "1h"
	}
	return cc
}

type AnthropicAdapter struct {
	version string
}

var (
	anthropicOnce    sync.Once
	anthropicAdapter *AnthropicAdapter
)

// GetAnthropicAdapter returns a singleton AnthropicAdapter instance.
func GetAnthropicAdapter() *AnthropicAdapter {
	anthropicOnce.Do(func() {
		anthropicAdapter = NewAnthropicAdapter()
	})
	return anthropicAdapter
}

// ConvertOpenAIToAnthropicBody converts an OpenAI-format request body
// (as a parsed map) to the Anthropic Messages API format. CacheOpts
// controls cache_control marker injection, per-breakpoint TTLs, and the
// request-varying trailer exclusions.
func (a *AnthropicAdapter) ConvertOpenAIToAnthropicBody(openai map[string]interface{}, model string, stream bool, opts CacheOpts) map[string]interface{} {
	return a.convertOpenAIToAnthropic(openai, model, stream, opts)
}

// ConvertAnthropicToOpenAIBody converts an Anthropic-format response
// (as a parsed map) to the OpenAI chat completions format.
func (a *AnthropicAdapter) ConvertAnthropicToOpenAIBody(anthropic map[string]interface{}, stream bool) map[string]interface{} {
	return a.convertAnthropicToOpenAI(anthropic, stream, nil)
}

// ConvertAnthropicToOpenAIBodyReported is ConvertAnthropicToOpenAIBody with a
// DropReport: content the OpenAI shape cannot represent (thinking-block
// signatures, redacted thinking, unknown block types) is recorded by kind on
// the report instead of being dropped silently (NENYA-176). Nil report
// behaves exactly like the unreported variant.
func (a *AnthropicAdapter) ConvertAnthropicToOpenAIBodyReported(anthropic map[string]interface{}, stream bool, report *DropReport) map[string]interface{} {
	return a.convertAnthropicToOpenAI(anthropic, stream, report)
}

// ConvertAnthropicRequestToOpenAIBody converts an Anthropic-format request body
// (as a parsed map) to the OpenAI chat completions format. This is the reverse
// of ConvertOpenAIToAnthropicBody and is used when clients send Anthropic-format
// requests to the gateway.
func (a *AnthropicAdapter) ConvertAnthropicRequestToOpenAIBody(anthropic map[string]interface{}) map[string]interface{} {
	return a.convertAnthropicToOpenAIRequest(anthropic, nil)
}

// ConvertAnthropicRequestToOpenAIBodyReported is ConvertAnthropicRequestToOpenAIBody
// with a DropReport for content the OpenAI shape cannot represent (NENYA-176).
func (a *AnthropicAdapter) ConvertAnthropicRequestToOpenAIBodyReported(anthropic map[string]interface{}, report *DropReport) map[string]interface{} {
	return a.convertAnthropicToOpenAIRequest(anthropic, report)
}

// ConvertOpenAIResponseToAnthropicBody converts an OpenAI-format response
// (as a parsed map) to the Anthropic Messages API format. This is the reverse
// of ConvertAnthropicToOpenAIBody and is used when the gateway needs to send
// Anthropic-format responses to clients that sent Anthropic-format requests.
func (a *AnthropicAdapter) ConvertOpenAIResponseToAnthropicBody(openai map[string]interface{}) map[string]interface{} {
	return a.convertOpenAIToAnthropicResponse(openai)
}

// NewAnthropicAdapter creates a new AnthropicAdapter instance.
func NewAnthropicAdapter() *AnthropicAdapter {
	return &AnthropicAdapter{
		version: "2023-06-01",
	}
}

// InjectAuth adds the Anthropic-specific authentication headers (x-api-key and anthropic-version).
func (a *AnthropicAdapter) InjectAuth(req *http.Request, apiKey string) error {
	if err := verifyAPIKey(apiKey, "anthropic"); err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Anthropic-Version", a.version)
	return nil
}

// MutateRequest converts an OpenAI-format request body to Anthropic format.
func (a *AnthropicAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	if len(body) > maxPreFlightBodyBytes {
		return nil, fmt.Errorf("anthropic: request body too large (%d bytes, max %d)", len(body), maxPreFlightBodyBytes)
	}

	var openai map[string]interface{}
	if err := json.Unmarshal(body, &openai); err != nil {
		slog.Debug("anthropic: request body is not valid JSON, passing through", "error", err)
		return body, nil
	}

	opts := CacheOpts{
		GlobalTTL: "ephemeral",
	}
	anthropic := a.convertOpenAIToAnthropic(openai, model, stream, opts)

	out, err := json.Marshal(anthropic)
	if err != nil {
		return body, fmt.Errorf("anthropic: failed to marshal converted request: %w", err)
	}
	return out, nil
}

// MutateResponse converts an Anthropic-format response to OpenAI format.
func (a *AnthropicAdapter) MutateResponse(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	var anthropic map[string]interface{}
	if err := json.Unmarshal(body, &anthropic); err != nil {
		slog.Debug("anthropic: response body is not valid JSON, passing through", "error", err)
		return body, nil
	}

	openai := a.convertAnthropicToOpenAI(anthropic, true, nil)

	out, err := json.Marshal(openai)
	if err != nil {
		return body, fmt.Errorf("anthropic: failed to marshal converted response: %w", err)
	}
	return out, nil
}

// NormalizeError classifies Anthropic HTTP errors into retryable, rate-limited, quota-exhausted, or permanent.
func (a *AnthropicAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	switch statusCode {
	case 429:
		return ErrorRateLimited
	case 500, 502, 503, 504:
		return ErrorRetryable
	case 400:
		lower := strings.ToLower(string(body))
		if strings.Contains(lower, "overloaded") || strings.Contains(lower, "rate") {
			return ErrorRetryable
		}
		return ErrorPermanent
	case 529:
		return ErrorRateLimited
	default:
		return ErrorPermanent
	}
}

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
			a.setSystemPrompt(openai, anthropic, false, opts.getSystemTTL(), systemHoist, midConvoSystem, opts.TrailingContextMarkers)
		}
		anthropic["cache_control"] = buildCacheControl(opts.getSystemTTL())
		return anthropic
	}

	if hasMessages {
		a.setSystemPrompt(openai, anthropic, opts.System, opts.getSystemTTL(), systemHoist, midConvoSystem, opts.TrailingContextMarkers)
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

func (a *AnthropicAdapter) setSystemPrompt(openai, anthropic map[string]interface{}, cacheSystem bool, ttl string, hoistCount int, midConvoSystem bool, markers []string) {
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
	claimed := make(map[string]interface{}, len(lastBlock)+1)
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
	for _, marker := range markers {
		if marker != "" && strings.Contains(text, "[internal-ref] "+marker) {
			return true
		}
	}
	return false
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

func (a *AnthropicAdapter) convertAnthropicToOpenAI(anthropic map[string]interface{}, stream bool, report *DropReport) map[string]interface{} {
	openai := map[string]interface{}{
		"id":      "anthropic-" + util.GenerateID(),
		"object":  "chat.completion",
		"created": 0,
		"model":   anthropic["model"],
	}

	choice := map[string]interface{}{
		"index": 0,
	}

	if stream {
		delta := map[string]interface{}{}
		a.processAnthropicContent(anthropic, delta, choice, report)
		a.processStopReason(anthropic, choice)
		choice["delta"] = delta
		openai["object"] = "chat.completion.chunk"
	} else {
		message := map[string]interface{}{
			"role": "assistant",
		}
		a.processAnthropicContent(anthropic, message, choice, report)
		a.processStopReason(anthropic, choice)
		choice["message"] = message
	}

	openai["choices"] = []interface{}{choice}
	a.processUsage(anthropic, openai)

	return openai
}

// reportThinkingBlock classifies one thinking-family block in a reverse
// translation (NENYA-176): its reasoning text is the surfaced
// representation, the opaque signature blob and redacted thinking are
// recorded by kind on the report (never dropped silently).
func reportThinkingBlock(block map[string]interface{}, report *DropReport) (string, bool) {
	reason, _ := block["thinking"].(string)
	if sig, _ := block["signature"].(string); sig != "" {
		report.Add("thinking_signature")
	}
	return reason, reason != ""
}

// untypedBlockKind names a block whose type is missing, non-string, or
// empty, so it is still reported instead of vanishing silently.
func untypedBlockKind(block map[string]interface{}) string {
	bType, _ := block["type"].(string)
	if bType == "" {
		return "unknown"
	}
	return bType
}

// extractContentBlocks extracts text parts, tool calls, and reasoning text
// from Anthropic content blocks. Content the OpenAI shape cannot represent is
// recorded on the report (nil-safe) rather than dropped silently (NENYA-176):
// text-bearing reasoning blocks surface as reasoning_content, redacted
// thinking and unknown block types are counted by kind.
func (a *AnthropicAdapter) extractContentBlocks(blocks []interface{}, report *DropReport) ([]string, []interface{}, []string) {
	var textParts []string
	var toolCalls []interface{}
	var reasonParts []string
	for _, block := range blocks {
		bm, ok := block.(map[string]interface{})
		if !ok {
			report.Add("unknown")
			continue
		}
		bType, _ := bm["type"].(string)
		switch bType {
		case "text":
			if t, ok := bm["text"].(string); ok {
				textParts = append(textParts, t)
			} else {
				// A text block without a text value is a malformed
				// fidelity loss: report, never drop silently.
				report.Add("text")
			}
		case "tool_use":
			toolCalls = append(toolCalls, a.convertToolUseBlock(bm))
		case "tool_result":
			if t, ok := bm["content"].(string); ok {
				textParts = append(textParts, t)
			} else {
				// Array-valued tool results have no flat OpenAI
				// representation here: report the loss.
				report.Add("tool_result")
			}
		case "thinking":
			if reason, ok := reportThinkingBlock(bm, report); ok {
				reasonParts = append(reasonParts, reason)
			} else if _, has := bm["signature"]; !has {
				// Empty thinking text with no signature: nothing was
				// surfaced at all — report rather than vanish.
				report.Add("thinking")
			}
		case "redacted_thinking":
			report.Add("redacted_thinking")
		default:
			report.Add(untypedBlockKind(bm))
			slog.Debug("Unknown Anthropic content block type", "type", bType)
		}
	}
	return textParts, toolCalls, reasonParts
}

func (a *AnthropicAdapter) processAnthropicContent(anthropic, delta, choice map[string]interface{}, report *DropReport) {
	content, ok := anthropic["content"]
	if !ok {
		return
	}

	switch c := content.(type) {
	case string:
		delta["content"] = c
		choice["finish_reason"] = "stop"
	case []interface{}:
		textParts, toolCalls, reasonParts := a.extractContentBlocks(c, report)
		if len(textParts) > 0 {
			delta["content"] = strings.Join(textParts, "")
		}
		if len(reasonParts) > 0 {
			delta["reasoning_content"] = strings.Join(reasonParts, "\n\n")
		}
		if len(toolCalls) > 0 {
			delta["tool_calls"] = toolCalls
			choice["finish_reason"] = "tool_calls"
		}
		if choice["finish_reason"] == nil {
			choice["finish_reason"] = "stop"
		}
	}
}

func (a *AnthropicAdapter) convertToolUseBlock(bm map[string]interface{}) map[string]interface{} {
	tc := map[string]interface{}{
		"id":   bm["id"],
		"type": "function",
		"function": map[string]interface{}{
			"name":      bm["name"],
			"arguments": "{}",
		},
	}
	if inp, ok := bm["input"]; ok {
		argsBytes, _ := json.Marshal(inp)
		tc["function"].(map[string]interface{})["arguments"] = string(argsBytes)
	}
	return tc
}

func (a *AnthropicAdapter) processStopReason(anthropic, choice map[string]interface{}) {
	stopReason, ok := anthropic["stop_reason"].(string)
	if !ok {
		return
	}

	switch stopReason {
	case "end_turn":
		choice["finish_reason"] = "stop"
	case "tool_use":
		choice["finish_reason"] = "tool_calls"
	case "max_tokens":
		choice["finish_reason"] = "length"
	case "refusal":
		choice["finish_reason"] = "content_filter"
	case "pause_turn":
		// Resumable terminal (NENYA-174): OpenAI semantics have no resume
		// verb, so a buffered OpenAI-facing client sees the turn end.
		choice["finish_reason"] = "stop"
	default:
		slog.Debug("Unknown Anthropic stop_reason, defaulting to 'stop'", "reason", stopReason)
		choice["finish_reason"] = "stop"
	}
}

// processUsage converts an Anthropic usage object into the OpenAI shape
// (NENYA-167). Anthropic reports input_tokens EXCLUDING the cache dimensions,
// while the OpenAI shape's prompt_tokens INCLUDES them with the cache counters
// as subsets; the conversion therefore folds the cache dimensions into the
// prompt total and re-publishes them as subsets under both the OpenAI-style
// keys and the native Anthropic spellings (matching the streaming
// transformer), so a client sees each dimension exactly once instead of
// losing the cache counts, whichever key family it reads.
func (a *AnthropicAdapter) processUsage(anthropic, openai map[string]interface{}) {
	usage, ok := anthropic["usage"].(map[string]interface{})
	if !ok {
		return
	}

	totals := stream.NormalizeUsageTotals(usage)
	// float64 values keep the converted map type-identical to a JSON-decoded
	// upstream usage object (NENYA-167).
	converted := map[string]interface{}{
		"prompt_tokens":     float64(totals.Prompt),
		"completion_tokens": float64(totals.Completion),
		"total_tokens":      float64(totals.Total),
	}
	if totals.CacheHit > 0 {
		converted["prompt_cache_hit_tokens"] = float64(totals.CacheHit)
		// Native Anthropic spelling, mirroring the streaming transformer
		// (internal/stream/anthropic_transformer.go) so buffered and
		// streaming clients see the same dimensions under the same keys.
		// Only the canonical totals are republished; re-emitting whatever
		// spelling the upstream used would leak provider dialects.
		converted["cache_read_input_tokens"] = float64(totals.CacheHit)
	}
	if totals.CacheMiss > 0 {
		converted["prompt_cache_miss_tokens"] = float64(totals.CacheMiss)
	}
	if totals.CacheCreation > 0 {
		converted["cache_creation_tokens"] = float64(totals.CacheCreation)
		converted["cache_creation_input_tokens"] = float64(totals.CacheCreation)
	}
	openai["usage"] = converted
}

func (a *AnthropicAdapter) convertAnthropicToOpenAIRequest(anthropic map[string]interface{}, report *DropReport) map[string]interface{} {
	openai := map[string]interface{}{}

	if model, ok := anthropic["model"].(string); ok {
		openai["model"] = model
	}

	if msgs, ok := anthropic["messages"].([]interface{}); ok {
		openai["messages"] = a.reverseConvertMessages(anthropic, msgs, report)
	}

	if tools, ok := anthropic["tools"].([]interface{}); ok && len(tools) > 0 {
		openai["tools"] = a.reverseConvertTools(tools)
	}

	if tc, ok := anthropic["tool_choice"]; ok {
		a.reverseConvertToolChoice(tc, openai)
	}

	a.copyAnthropicFields(anthropic, openai)

	return openai
}

func (a *AnthropicAdapter) copyAnthropicFields(anthropic, openai map[string]interface{}) {
	if v, ok := anthropic["max_tokens"]; ok {
		openai["max_tokens"] = v
	}
	if v, ok := anthropic["temperature"]; ok {
		openai["temperature"] = v
	}
	if v, ok := anthropic["top_p"]; ok {
		openai["top_p"] = v
	}
	if v, ok := anthropic["top_k"]; ok {
		openai["top_k"] = v
	}
	if v, ok := anthropic["stop_sequences"]; ok {
		openai["stop"] = v
	}
	if v, ok := anthropic["stream"].(bool); ok {
		openai["stream"] = v
	}
	if meta, ok := anthropic["metadata"].(map[string]interface{}); ok {
		if uid, ok := meta["user_id"].(string); ok {
			openai["user"] = uid
		}
	}
}

func (a *AnthropicAdapter) reverseConvertMessages(anthropic map[string]interface{}, msgs []interface{}, report *DropReport) []interface{} {
	var result []interface{}

	if systemContent := anthropic["system"]; systemContent != nil {
		sysMsg := a.buildOpenAISystemMessage(systemContent, report)
		if sysMsg != nil {
			result = append(result, sysMsg)
		}
	}

	for i := 0; i < len(msgs); i++ {
		msg, ok := msgs[i].(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := msg["role"].(string)

		if role == "user" {
			result = a.processUserMessage(result, msgs, msg, &i, report)
			continue
		}

		if role == "assistant" {
			result = append(result, a.buildOpenAIAssistantMessage(msg, report))
			continue
		}

		oMsg := map[string]interface{}{"role": role}
		if content, ok := msg["content"]; ok {
			if role == "system" {
				// System reminders move through the OpenAI pipeline and
				// back on Anthropic-format egress; preserve blocks
				// verbatim so cache_control breakpoints are not lost.
				oMsg["content"] = preserveSystemContent(content)
			} else {
				oMsg["content"] = a.contentBlocksToString(content)
				// NENYA-207: the flattening dropped any block-level mark —
				// hoist its presence onto the message (see hoistedCacheControl).
				hoistOntoMessage(oMsg, content)
			}
		}
		result = append(result, oMsg)
	}

	return result
}

func (a *AnthropicAdapter) processUserMessage(result []interface{}, msgs []interface{}, msg map[string]interface{}, i *int, report *DropReport) []interface{} {
	toolMsgs := a.extractToolResultMessages(msgs, i, report)
	if len(toolMsgs) > 0 {
		result = append(result, toolMsgs...)
		result = a.appendRemainingUserMessage(result, msgs, i, report)
		return result
	}
	return append(result, a.buildOpenAIUserMessage(msg, report))
}

func (a *AnthropicAdapter) appendRemainingUserMessage(result []interface{}, msgs []interface{}, i *int, report *DropReport) []interface{} {
	if *i >= len(msgs) {
		(*i)--
		return result
	}
	msg, ok := msgs[*i].(map[string]interface{})
	if !ok {
		(*i)--
		return result
	}
	role, _ := msg["role"].(string)
	if role == "user" {
		return append(result, a.buildOpenAIUserMessage(msg, report))
	}
	(*i)--
	return result
}

// buildOpenAISystemMessage flattens Anthropic system content to an OpenAI
// system message. Blocks without a text value carry nothing into the target
// shape: when the report is set, each such block (or a wholly
// unrepresentable system turn) is recorded rather than dropped silently
// (NENYA-176). Returns nil when there is nothing to send.
func (a *AnthropicAdapter) buildOpenAISystemMessage(systemContent interface{}, report *DropReport) map[string]interface{} {
	switch sc := systemContent.(type) {
	case string:
		if sc != "" {
			return map[string]interface{}{
				"role":    "system",
				"content": sc,
			}
		}
	case []interface{}:
		var parts []string
		for _, blockRaw := range sc {
			block, ok := blockRaw.(map[string]interface{})
			if !ok {
				report.Add("unknown")
				continue
			}
			bType, _ := block["type"].(string)
			if t, ok := block["text"].(string); ok && t != "" {
				parts = append(parts, t)
				continue
			}
			if bType == "text" {
				// A text block carrying no text value.
				report.Add("text")
				continue
			}
			// A non-text system block (a document attachment, a
			// foreign blob) has no OpenAI system representation.
			report.Add(untypedBlockKind(block))
		}
		if len(parts) > 0 {
			oMsg := map[string]interface{}{
				"role":    "system",
				"content": strings.Join(parts, "\n\n"),
			}
			// NENYA-207: the flattened system string dropped any block-level
			// mark — hoist its presence onto the message.
			hoistOntoMessage(oMsg, sc)
			return oMsg
		}
	default:
		// Neither string nor array: the whole system content has no
		// representation.
		report.Add("system")
	}
	return nil
}

// extractToolResultMessages checks if a user message contains only tool_result
// blocks. If so, it converts them to OpenAI tool-role messages and peeks ahead
// for more consecutive user messages that are purely tool_results. It advances
// *i past consumed messages and decrements if a non-tool-result user message
// is encountered so the caller can process it.
func (a *AnthropicAdapter) extractToolResultMessages(msgs []interface{}, i *int, report *DropReport) []interface{} {
	msg, ok := msgs[*i].(map[string]interface{})
	if !ok {
		return nil
	}

	content := msg["content"]
	if !a.isPureToolResults(content) {
		return nil
	}

	var result []interface{}
	for *i < len(msgs) {
		msg, ok := msgs[*i].(map[string]interface{})
		if !ok {
			break
		}
		role, _ := msg["role"].(string)
		if role != "user" {
			(*i)--
			break
		}
		content := msg["content"]
		if !a.isPureToolResults(content) {
			(*i)--
			break
		}
		result = append(result, a.convertToolResultContent(content, report)...)
		(*i)++
	}
	return result
}

func (a *AnthropicAdapter) isPureToolResults(content interface{}) bool {
	blocks, ok := content.([]interface{})
	if !ok {
		return false
	}
	if len(blocks) == 0 {
		return false
	}
	for _, blockRaw := range blocks {
		block, ok := blockRaw.(map[string]interface{})
		if !ok {
			return false
		}
		if typ, _ := block["type"].(string); typ != "tool_result" {
			return false
		}
	}
	return true
}

func (a *AnthropicAdapter) convertToolResultContent(content interface{}, report *DropReport) []interface{} {
	blocks, ok := content.([]interface{})
	if !ok {
		return nil
	}
	var result []interface{}
	for _, blockRaw := range blocks {
		block, ok := blockRaw.(map[string]interface{})
		if !ok {
			report.Add("unknown")
			continue
		}
		a.reportToolResultSubblocks(block["content"], report)
		toolUseID, _ := block["tool_use_id"].(string)
		toolContent := a.extractToolResultText(block["content"])
		oMsg := map[string]interface{}{
			"role":         "tool",
			"tool_call_id": toolUseID,
			"content":      toolContent,
		}
		// NENYA-207: the flattening destroyed a client mark on the
		// tool_result block — hoist its presence onto the converted tool
		// message so the stand-down scan sees the placement intent.
		if mark, ok := block["cache_control"]; ok {
			oMsg["cache_control"] = mark
		}
		result = append(result, oMsg)
	}
	return result
}

// reportToolResultSubblocks records the sub-block fidelity of an
// array-valued tool_result: only its text reaches the target shape, so
// non-text sub-blocks (images, documents, foreign blobs) are reported by
// kind rather than dropped silently (NENYA-176).
func (a *AnthropicAdapter) reportToolResultSubblocks(content interface{}, report *DropReport) {
	blocks, ok := content.([]interface{})
	if !ok {
		return
	}
	for _, blockRaw := range blocks {
		block, ok := blockRaw.(map[string]interface{})
		if !ok {
			report.Add("unknown")
			continue
		}
		bType, _ := block["type"].(string)
		switch bType {
		case "text":
			if _, isStr := block["text"].(string); !isStr {
				report.Add("text")
			}
		default:
			report.Add(untypedBlockKind(block))
		}
	}
}

// extractToolResultText reduces tool_result content (a string or an array of
// blocks) to plain text: string content passes through verbatim, array
// content keeps the text blocks, and anything else is stringified.
func (a *AnthropicAdapter) extractToolResultText(content interface{}) string {
	if content == nil {
		return ""
	}
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		var parts []string
		for _, blockRaw := range c {
			if block, ok := blockRaw.(map[string]interface{}); ok {
				if typ, _ := block["type"].(string); typ == "text" {
					if t, ok := block["text"].(string); ok {
						parts = append(parts, t)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return fmt.Sprintf("%v", content)
}

func (a *AnthropicAdapter) buildOpenAIUserMessage(msg map[string]interface{}, report *DropReport) map[string]interface{} {
	content := msg["content"]
	switch c := content.(type) {
	case string:
		oMsg := map[string]interface{}{
			"role":    "user",
			"content": c,
		}
		if m, ok := msg["cache_control"]; ok {
			oMsg["cache_control"] = m
		}
		return oMsg
	case []interface{}:
		parts := a.convertContentBlocksToOpenAI(c, report)
		var oMsg map[string]interface{}
		if text := a.extractSingleText(parts); text != nil {
			// NENYA-207: the flattened string dropped any block-level mark.
			oMsg = map[string]interface{}{
				"role":    "user",
				"content": *text,
			}
		} else {
			// The parts were rebuilt without cache_control keys
			// (convertContentBlocksToOpenAI) — same hoist.
			oMsg = map[string]interface{}{
				"role":    "user",
				"content": parts,
			}
		}
		// A message-level mark on the input rides along and wins over a
		// hoisted block mark.
		if m, ok := msg["cache_control"]; ok {
			oMsg["cache_control"] = m
		}
		hoistOntoMessage(oMsg, content)
		return oMsg
	default:
		oMsg := map[string]interface{}{
			"role":    "user",
			"content": fmt.Sprintf("%v", content),
		}
		// A message-level mark on the input rides along (parity with the
		// string and array paths, NENYA-207).
		if m, ok := msg["cache_control"]; ok {
			oMsg["cache_control"] = m
		}
		return oMsg
	}
}

func (a *AnthropicAdapter) extractSingleText(parts []interface{}) *string {
	if len(parts) != 1 {
		return nil
	}
	text, ok := parts[0].(map[string]interface{})
	if !ok {
		return nil
	}
	if typ, _ := text["type"].(string); typ != "text" {
		return nil
	}
	t, ok := text["text"].(string)
	if !ok {
		return nil
	}
	return &t
}

// convertContentBlocksToOpenAI maps Anthropic content blocks to OpenAI parts.
// Foreign block types (documents, attachments, anything the OpenAI shape
// cannot carry) are recorded on the report — never dropped silently
// (NENYA-176).
func (a *AnthropicAdapter) convertContentBlocksToOpenAI(blocks []interface{}, report *DropReport) []interface{} {
	var result []interface{}
	for _, blockRaw := range blocks {
		block, ok := blockRaw.(map[string]interface{})
		if !ok {
			report.Add("unknown")
			continue
		}
		bType, _ := block["type"].(string)
		switch bType {
		case "text":
			if text, ok := block["text"].(string); ok {
				result = append(result, map[string]interface{}{
					"type": "text",
					"text": text,
				})
			} else {
				report.Add("text")
			}
		case "image":
			if source, ok := block["source"].(map[string]interface{}); ok {
				result = append(result, a.convertImageSourceToOpenAI(source))
			} else {
				// An image block without a source map has no
				// representation: report the loss.
				report.Add("image")
			}
		case "tool_result":
			// Mixed content: extractToolResultMessages only drains PURE
			// tool-result turns, so a tool_result inside a text-bearing
			// user turn lands here — keep its text instead of dropping it
			// (NENYA-176).
			a.reportToolResultSubblocks(block["content"], report)
			if text := a.extractToolResultText(block["content"]); text != "" {
				result = append(result, map[string]interface{}{
					"type": "text",
					"text": text,
				})
			} else {
				report.Add("tool_result")
			}
		case "tool_use":
			// A tool_use block cannot appear in a client-provided user
			// turn; report rather than drop silently.
			report.Add("tool_use")
		default:
			report.Add(untypedBlockKind(block))
		}
	}
	return result
}

func (a *AnthropicAdapter) convertImageSourceToOpenAI(source map[string]interface{}) map[string]interface{} {
	if srcType, _ := source["type"].(string); srcType == "base64" {
		mediaType, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		if mediaType != "" && data != "" {
			return map[string]interface{}{
				"type": "image_url",
				"image_url": map[string]interface{}{
					"url": fmt.Sprintf("data:%s;base64,%s", mediaType, data),
				},
			}
		}
	}
	return map[string]interface{}{
		"type":   "image",
		"source": source,
	}
}

func (a *AnthropicAdapter) buildOpenAIAssistantMessage(msg map[string]interface{}, report *DropReport) map[string]interface{} {
	textParts, toolCalls, reasonParts := a.extractAssistantContent(msg["content"], report)

	oMsg := map[string]interface{}{
		"role": "assistant",
	}
	// A message-level mark on the input rides along; a block-level one is
	// hoisted when the flattening drops it (NENYA-207).
	if m, ok := msg["cache_control"]; ok {
		oMsg["cache_control"] = m
	}
	hoistOntoMessage(oMsg, msg["content"])
	if len(textParts) > 0 {
		oMsg["content"] = strings.Join(textParts, "")
	} else if len(toolCalls) > 0 {
		oMsg["content"] = ""
	}
	if len(reasonParts) > 0 {
		oMsg["reasoning_content"] = strings.Join(reasonParts, "\n\n")
	}

	if len(toolCalls) > 0 {
		oMsg["tool_calls"] = toolCalls
	}

	return oMsg
}

// extractAssistantContent maps an Anthropic assistant content array to OpenAI
// text parts and tool calls. Thinking blocks surface as OpenAI reasoning
// content (their signatures are opaque to the OpenAI shape and recorded on
// the report); redacted thinking and unknown block types are reported rather
// than dropped silently (NENYA-176).
func (a *AnthropicAdapter) extractAssistantContent(content interface{}, report *DropReport) ([]string, []interface{}, []string) {
	var textParts []string
	var toolCalls []interface{}
	var reasonParts []string

	blocks, ok := content.([]interface{})
	if !ok {
		if text, ok := content.(string); ok {
			return []string{text}, nil, nil
		}
		// Neither array nor string: the whole content has no
		// representation — report rather than vanish (NENYA-176).
		report.Add("unknown")
		return nil, nil, nil
	}

	for _, blockRaw := range blocks {
		block, ok := blockRaw.(map[string]interface{})
		if !ok {
			report.Add("unknown")
			continue
		}
		bType, _ := block["type"].(string)
		switch bType {
		case "text":
			if t, ok := block["text"].(string); ok {
				if t != "" {
					textParts = append(textParts, t)
				}
			} else {
				// Non-string text value: a malformed fidelity loss.
				report.Add("text")
			}
		case "tool_use":
			tc := a.reverseConvertToolUseBlock(block)
			if tc != nil {
				toolCalls = append(toolCalls, tc)
			}
		case "thinking":
			if reason, ok := reportThinkingBlock(block, report); ok {
				reasonParts = append(reasonParts, reason)
			} else if _, has := block["signature"]; !has {
				report.Add("thinking")
			}
		case "redacted_thinking":
			report.Add("redacted_thinking")
		default:
			report.Add(untypedBlockKind(block))
		}
	}
	return textParts, toolCalls, reasonParts
}

func (a *AnthropicAdapter) reverseConvertToolUseBlock(block map[string]interface{}) map[string]interface{} {
	id, _ := block["id"].(string)
	name, _ := block["name"].(string)

	var argsStr string
	if input, ok := block["input"]; ok {
		argsBytes, err := json.Marshal(input)
		if err == nil {
			argsStr = string(argsBytes)
		}
	}
	if argsStr == "" {
		argsStr = "{}"
	}

	return map[string]interface{}{
		"id":   id,
		"type": "function",
		"function": map[string]interface{}{
			"name":      name,
			"arguments": argsStr,
		},
	}
}

func (a *AnthropicAdapter) reverseConvertTools(tools []interface{}) []interface{} {
	var result []interface{}
	for _, toolRaw := range tools {
		tool, ok := toolRaw.(map[string]interface{})
		if !ok {
			continue
		}

		name, _ := tool["name"].(string)
		desc, _ := tool["description"].(string)

		result = append(result, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        name,
				"description": desc,
				// Sanitized both ways (NENYA-172).
				"parameters": jsonschema.Sanitize(tool["input_schema"]),
			},
		})
	}
	return result
}

func (a *AnthropicAdapter) reverseConvertToolChoice(tc interface{}, openai map[string]interface{}) {
	if m, ok := tc.(map[string]interface{}); ok {
		tcType, _ := m["type"].(string)
		switch tcType {
		case "auto", "required":
			openai["tool_choice"] = tcType
		case "tool":
			if name, ok := m["name"].(string); ok {
				openai["tool_choice"] = map[string]interface{}{
					"type": "function",
					"function": map[string]interface{}{
						"name": name,
					},
				}
			}
		}
		return
	}
	if s, ok := tc.(string); ok {
		openai["tool_choice"] = s
	}
}

// preserveSystemContent keeps system-message content intact when it is
// already structured: Anthropic-native clients attach cache_control
// breakpoints to mid-conversation system reminders, and flattening them
// to a string would drop the breakpoint and freeze provider prompt
// caching at the static prefix. Strings pass through untouched; any
// other shape falls back to the lossy text extraction.
func preserveSystemContent(content interface{}) interface{} {
	switch content.(type) {
	case string, []interface{}:
		return content
	default:
		return ""
	}
}

// hoistedCacheControl preserves a client cache_control breakpoint's PRESENCE
// when the reverse conversion flattens Anthropic content blocks (NENYA-207):
// the OpenAI shape carries a string where Anthropic carried typed blocks, so
// a block-level mark cannot survive positionally. Hoisting the first marked
// block's cache_control onto the message keeps the auto-anchor stand-down
// scan (hasClientCacheControl walks the whole payload, message level
// included) honest — without it the gateway would anchor its own placement
// over the client's destroyed placement and record "injected". nil when no
// block is marked.
func hoistedCacheControl(content interface{}) interface{} {
	blocks, ok := content.([]interface{})
	if !ok {
		return nil
	}
	for _, blockRaw := range blocks {
		block, ok := blockRaw.(map[string]interface{})
		if !ok {
			continue
		}
		if mark, ok := block["cache_control"]; ok {
			return mark
		}
	}
	return nil
}

// hoistOntoMessage attaches a hoisted cache_control mark to an OpenAI-shaped
// message unless it already carries one (a client-placed message-level mark
// wins over a hoisted block mark).
func hoistOntoMessage(oMsg map[string]interface{}, content interface{}) {
	if _, exists := oMsg["cache_control"]; exists {
		return
	}
	if mark := hoistedCacheControl(content); mark != nil {
		oMsg["cache_control"] = mark
	}
}

func (a *AnthropicAdapter) contentBlocksToString(content interface{}) interface{} {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		var parts []string
		for _, blockRaw := range c {
			if block, ok := blockRaw.(map[string]interface{}); ok {
				if typ, _ := block["type"].(string); typ == "text" {
					if t, ok := block["text"].(string); ok {
						parts = append(parts, t)
					}
				}
			}
		}
		if len(parts) == 1 {
			return parts[0]
		}
		if len(parts) > 1 {
			return strings.Join(parts, "")
		}
	}
	return ""
}

func (a *AnthropicAdapter) convertOpenAIToAnthropicResponse(openai map[string]interface{}) map[string]interface{} {
	anthropic := map[string]interface{}{
		"id":   a.extractResponseID(openai),
		"type": "message",
		"role": "assistant",
	}

	if model, ok := openai["model"]; ok {
		anthropic["model"] = model
	}

	var contentBlocks []interface{}
	var stopReason string

	choices, ok := openai["choices"].([]interface{})
	if ok && len(choices) > 0 {
		choice, ok := choices[0].(map[string]interface{})
		if ok {
			contentBlocks, stopReason = a.extractOpenAIChoiceContent(choice)
		}
	}

	if len(contentBlocks) == 0 {
		contentBlocks = []interface{}{map[string]interface{}{"type": "text", "text": ""}}
	}
	anthropic["content"] = contentBlocks

	if stopReason != "" {
		anthropic["stop_reason"] = stopReason
	}

	// Anthropic's input_tokens excludes the cache dimensions, which are
	// reported separately (NENYA-167): the OpenAI prompt total is split so a
	// consumer that sums input + cache_read + cache_creation gets the total
	// exactly once.
	if usage, ok := openai["usage"].(map[string]interface{}); ok {
		totals := stream.NormalizeUsageTotals(usage)
		converted := map[string]interface{}{
			"input_tokens":  float64(max(0, totals.Prompt-totals.CacheHit-totals.CacheCreation)),
			"output_tokens": float64(totals.Completion),
		}
		if totals.CacheHit > 0 {
			converted["cache_read_input_tokens"] = float64(totals.CacheHit)
		}
		if totals.CacheCreation > 0 {
			converted["cache_creation_input_tokens"] = float64(totals.CacheCreation)
		}
		anthropic["usage"] = converted
	}

	return anthropic
}

func (a *AnthropicAdapter) extractResponseID(openai map[string]interface{}) string {
	if id, ok := openai["id"].(string); ok && id != "" {
		return id
	}
	return "msg_" + util.GenerateID()
}

// extractOpenAIChoiceContent extracts the Anthropic content blocks and stop
// reason for one OpenAI-format choice. Reasoning rides as visible text
// (NENYA-176), array content is flattened via appendOpenAIContentArray.
func (a *AnthropicAdapter) extractOpenAIChoiceContent(choice map[string]interface{}) ([]interface{}, string) {
	var contentBlocks []interface{}
	var stopReason string

	message := choice
	if msg, ok := choice["message"].(map[string]interface{}); ok {
		message = msg
	}

	if reason, _ := message["reasoning_content"].(string); reason != "" {
		// Foreign reasoning cannot ride an unsigned `thinking` block:
		// replaying such a block as history to an Anthropic upstream is a
		// guaranteed 400 (signature required — see ADAPTERS.md's
		// invariant table). The reasoning rides as visible, sendable text
		// instead of being dropped silently (NENYA-176).
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type": "text",
			"text": "[reasoning] " + reason,
		})
	}

	switch c := message["content"].(type) {
	case string:
		if c != "" {
			contentBlocks = append(contentBlocks, map[string]interface{}{
				"type": "text",
				"text": c,
			})
		}
	case []interface{}:
		contentBlocks = a.appendOpenAIContentArray(contentBlocks, c)
	default:
		// Non-string/non-array content has no representation; leave it
		// unrepresented rather than guessing.
	}

	if toolCalls, ok := message["tool_calls"].([]interface{}); ok {
		for _, tcRaw := range toolCalls {
			tc, ok := tcRaw.(map[string]interface{})
			if !ok {
				continue
			}
			block := a.convertToolCallToAnthropic(tc)
			if block != nil {
				contentBlocks = append(contentBlocks, block)
			}
		}
	}

	if fr, ok := choice["finish_reason"].(string); ok {
		stopReason = a.mapFinishReason(fr)
	}

	return contentBlocks, stopReason
}

// appendOpenAIContentArray flattens an array-shaped OpenAI response content
// into Anthropic blocks (multi-part / OpenRouter-style). Text parts are
// extracted; unsendable non-text parts carry the visible
// "[unsendable content: <type>]" marker instead of vanishing (NENYA-176).
func (a *AnthropicAdapter) appendOpenAIContentArray(contentBlocks []interface{}, parts []interface{}) []interface{} {
	for _, partRaw := range parts {
		part, ok := partRaw.(map[string]interface{})
		if !ok {
			contentBlocks = append(contentBlocks, map[string]interface{}{
				"type": "text",
				"text": "[unsendable content: unknown]",
			})
			continue
		}
		partType, _ := part["type"].(string)
		if text, ok := part["text"].(string); ok && partType == "text" {
			contentBlocks = append(contentBlocks, map[string]interface{}{
				"type": "text",
				"text": text,
			})
			continue
		}
		if partType == "" {
			partType = "unknown"
		}
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type": "text",
			"text": "[unsendable content: " + partType + "]",
		})
	}
	return contentBlocks
}

func (a *AnthropicAdapter) convertToolCallToAnthropic(tc map[string]interface{}) map[string]interface{} {
	id, _ := tc["id"].(string)
	fn, ok := tc["function"].(map[string]interface{})
	if !ok {
		return nil
	}
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

func (a *AnthropicAdapter) mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return reason
	}
}
