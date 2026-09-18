package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/stream"
)

var GeminiModelMap = map[string]string{
	"gemini-3-flash":        "gemini-3-flash-preview",
	"gemini-3.5-flash":      "gemini-3.5-flash",
	"gemini-3.1-flash":      "gemini-3.1-flash-preview",
	"gemini-3.1-flash-lite": "gemini-3.1-flash-lite-preview",
	"gemini-3.1-pro":        "gemini-3.1-pro-preview",
	"gemini-flash":          "gemini-2.5-flash",
	"gemini-flash-lite":     "gemini-2.5-flash-lite",
	"gemini-pro":            "gemini-2.5-pro",
}

func geminiSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:           []ServiceKind{ServiceKindLLM},
		ModelMap:               GeminiModelMap,
		SanitizeRequest:        geminiSanitize,
		NewResponseTransformer: newGeminiTransformer,
		ValidationEndpoint:     geminiValidationEndpoint,
	}
}

type GeminiTransformer struct {
	OnExtraContent func(toolCallID string, extraContent interface{})
	// pendingExtra tracks the last tool_call id seen per "choice:tool" index
	// so a signature arriving in a later delta chunk (after the id chunk) is
	// still associated with the right call (NENYA-51 split-delta gap).
	pendingExtra map[string]string
}

func newGeminiTransformer(cache *infra.ThoughtSignatureCache) stream.ResponseTransformer {
	return &GeminiTransformer{
		OnExtraContent: func(toolCallID string, extraContent interface{}) {
			if cache != nil {
				cache.Store(toolCallID, extraContent)
			}
		},
		pendingExtra: make(map[string]string),
	}
}

func (t *GeminiTransformer) TransformSSEChunk(ctx context.Context, data []byte) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if len(data) == 0 || !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return data, nil
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return data, nil
	}

	t.processToolCalls(chunk)

	transformed, err := json.Marshal(chunk)
	if err != nil {
		return data, fmt.Errorf("failed to marshal transformed chunk: %v", err)
	}

	return transformed, nil
}

func (t *GeminiTransformer) processToolCalls(chunk map[string]interface{}) {
	choices, ok := chunk["choices"].([]interface{})
	if !ok {
		return
	}

	// NENYA-51: iterate ALL choices (n>1 streams previously lost caching
	// beyond choices[0]).
	for ci, choiceRaw := range choices {
		choice, ok := choiceRaw.(map[string]interface{})
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]interface{})
		if !ok {
			continue
		}
		toolCalls, ok := delta["tool_calls"].([]interface{})
		if !ok {
			continue
		}
		for i, tc := range toolCalls {
			tcMap, ok := tc.(map[string]interface{})
			if !ok {
				continue
			}
			if _, exists := tcMap["index"]; !exists {
				tcMap["index"] = i
			}
			t.handleExtraContent(fmt.Sprintf("%d:%d", ci, i), tcMap)
		}
	}
}

// handleExtraContent stores a tool call's thought signature. The signature
// may arrive in the same delta as the id or in a later delta chunk split by
// index; pendingExtra bridges the two shapes.
func (t *GeminiTransformer) handleExtraContent(pendingKey string, tcMap map[string]interface{}) {
	if t.OnExtraContent == nil {
		return
	}

	tcID, _ := tcMap["id"].(string)
	if tcID != "" {
		if t.pendingExtra == nil {
			t.pendingExtra = make(map[string]string)
		}
		t.pendingExtra[pendingKey] = tcID
	} else {
		tcID = t.pendingExtra[pendingKey]
	}
	if tcID == "" {
		return
	}

	extra, hasExtra := tcMap["extra_content"]
	if !hasExtra {
		return
	}

	t.OnExtraContent(tcID, extra)
}

// geminiLogger is a minimal logger interface used by Gemini provider helpers.
// Defined here to avoid circular dependencies with internal/adapter.
type geminiLogger interface {
	Debug(msg string, args ...any)
	Warn(msg string, args ...any)
}

// isGemini3OrNewer checks if the model ID indicates a Gemini 3.x or newer model.
func isGemini3OrNewer(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "gemini-3")
}

// isGemini35OrNewer checks if the model ID indicates a Gemini 3.5 or 4 model.
// These models require forwarding function call IDs from tool calls to tool responses.
func isGemini35OrNewer(model string) bool {
	modelLower := strings.ToLower(model)
	return strings.Contains(modelLower, "gemini-3.5") || strings.Contains(modelLower, "gemini-4")
}

// mapReasoningEffortToLevel converts a reasoning_effort value to a Gemini
// thinkingLevel string for Gemini 3+ models. Unknown values default to "medium".
func mapReasoningEffortToLevel(reasoningEffort string, logger geminiLogger) string {
	var level string
	switch strings.ToLower(reasoningEffort) {
	case "none", "disable":
		level = "minimal"
	case "low", "minimal":
		level = "low"
	case "medium":
		level = "medium"
	case "high":
		level = "high"
	default:
		if logger != nil {
			logger.Warn("gemini: unknown reasoning_effort value, using default", "value", reasoningEffort, "default", "medium")
		}
		level = "medium"
	}
	return level
}

// mapReasoningEffortToBudget converts a reasoning_effort value to a Gemini
// thinkingBudget token count for Gemini 2.5 models. Unknown values default to 8192.
func mapReasoningEffortToBudget(reasoningEffort string, logger geminiLogger) int {
	var budget int
	switch strings.ToLower(reasoningEffort) {
	case "none", "disable":
		budget = 0
	case "low", "minimal":
		budget = 1024
	case "medium":
		budget = 8192
	case "high":
		budget = 24576
	default:
		if logger != nil {
			logger.Warn("gemini: unknown reasoning_effort value, using default", "value", reasoningEffort, "default", "medium")
		}
		budget = 8192
	}
	return budget
}

// injectThinkingForGemini maps reasoning_effort to Gemini's thinking config.
// For Gemini 3+ models it uses thinkingLevel (enum); for older models (Gemini 2.5)
// it uses thinkingBudget (token count). Removes reasoning_effort from the payload.
func injectThinkingForGemini(deps *SanitizeDeps, payload map[string]interface{}) {
	model, _ := payload["model"].(string)
	if model == "" {
		return
	}
	reasoningEffortRaw, hasReasoning := payload["reasoning_effort"]
	if !hasReasoning {
		return
	}
	reasoningEffort, ok := reasoningEffortRaw.(string)
	if !ok {
		if deps.Logger != nil {
			deps.Logger.Warn("gemini: reasoning_effort must be a string", "type", fmt.Sprintf("%T", reasoningEffortRaw))
		}
		return
	}

	var thinkingConfig map[string]interface{}

	if isGemini3OrNewer(model) {
		level := mapReasoningEffortToLevel(reasoningEffort, deps.Logger)
		thinkingConfig = map[string]interface{}{"thinkingLevel": level}
	} else {
		budget := mapReasoningEffortToBudget(reasoningEffort, deps.Logger)
		thinkingConfig = map[string]interface{}{"thinkingBudget": budget}
	}

	google := map[string]interface{}{"thinking_config": thinkingConfig}
	if extraBody, ok := payload["extra_body"].(map[string]interface{}); ok {
		if existing, ok := extraBody["google"].(map[string]interface{}); ok {
			existing["thinking_config"] = thinkingConfig
		} else {
			extraBody["google"] = google
		}
	} else {
		payload["extra_body"] = map[string]interface{}{"google": google}
	}
	delete(payload, "reasoning_effort")
	if deps.Logger != nil {
		deps.Logger.Debug("gemini: mapped reasoning_effort to thinking config", "model", model, "reasoning_effort", reasoningEffort)
	}
}

// injectTemperatureDefaultsForGemini sets model-specific temperature defaults.
// Gemini 3+ models require temperature=1.0 to prevent looping behavior on reasoning tasks.
func injectTemperatureDefaultsForGemini(payload map[string]interface{}) {
	model, _ := payload["model"].(string)
	if model == "" {
		return
	}
	if !isGemini3OrNewer(model) {
		return
	}
	if _, hasTemp := payload["temperature"]; hasTemp {
		return
	}
	payload["temperature"] = 1.0
}

// forwardFunctionCallIDs forwards tool call IDs from assistant messages to tool messages.
// Gemini 3.5+ returns unique id fields with every functionCall and requires echoing them in functionResponse.
// This function builds a map of tool_call_id → functionName from assistant messages, then injects the name field
// into tool messages that reference those IDs.
func forwardFunctionCallIDs(deps *SanitizeDeps, messages []interface{}) {
	toolCallIDToName := make(map[string]string)
	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		toolCallsRaw, ok := msg["tool_calls"]
		if !ok {
			continue
		}
		toolCalls, ok := toolCallsRaw.([]interface{})
		if !ok {
			continue
		}
		for _, tcRaw := range toolCalls {
			tc, ok := tcRaw.(map[string]interface{})
			if !ok {
				continue
			}
			tcID, _ := tc["id"].(string)
			if tcID != "" {
				fnName := geminiExtractFunctionName(tc)
				toolCallIDToName[tcID] = fnName
			}
		}
	}
	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "tool" {
			continue
		}
		tcID, _ := msg["tool_call_id"].(string)
		if tcID != "" {
			if fnName, ok := toolCallIDToName[tcID]; ok {
				msg["name"] = fnName
			}
		}
	}
}

func geminiSanitize(deps *SanitizeDeps, payload map[string]interface{}) {
	injectThinkingForGemini(deps, payload)
	injectTemperatureDefaultsForGemini(payload)
	messagesRaw, ok := payload["messages"]
	if !ok {
		return
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok {
		return
	}

	if demoteGeminiMidSessionSystem(deps, messages) {
		payload["messages"] = messages
	}

	toolCallMap := geminiBuildToolCallMap(deps, messages)
	orphanedIDs := geminiIdentifyOrphanedIDs(deps, toolCallMap)

	if len(orphanedIDs) == 0 {
		geminiInjectFunctionNames(deps, messages, toolCallMap)
		return
	}

	filtered := geminiFilterMessages(deps, messages, toolCallMap, orphanedIDs)
	if len(filtered) != len(messages) {
		payload["messages"] = filtered
	}

	model, _ := payload["model"].(string)
	if model != "" && isGemini35OrNewer(model) {
		forwardFunctionCallIDs(deps, filtered)
	}
}

type toolCallInfo struct {
	id       string
	name     string
	hasExtra bool
}

func geminiBuildToolCallMap(deps *SanitizeDeps, messages []interface{}) map[string]*toolCallInfo {
	toolCallMap := make(map[string]*toolCallInfo)

	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}

		toolCallsRaw, ok := msg["tool_calls"]
		if !ok {
			continue
		}
		toolCalls, ok := toolCallsRaw.([]interface{})
		if !ok {
			continue
		}

		for _, tcRaw := range toolCalls {
			tc, ok := tcRaw.(map[string]interface{})
			if !ok {
				continue
			}
			tcID, _ := tc["id"].(string)
			if tcID == "" {
				continue
			}

			hasExtra := geminiEnsureExtraContent(deps, tc, tcID)
			fnName := geminiExtractFunctionName(tc)

			toolCallMap[tcID] = &toolCallInfo{
				id:       tcID,
				name:     fnName,
				hasExtra: hasExtra,
			}
		}
	}

	return toolCallMap
}

func geminiEnsureExtraContent(deps *SanitizeDeps, tc map[string]interface{}, tcID string) bool {
	_, hasExtra := tc["extra_content"]
	if hasExtra {
		return true
	}

	if deps.ThoughtSigCache == nil {
		return false
	}

	cached, found := deps.ThoughtSigCache.Load(tcID)
	if !found {
		return false
	}

	tc["extra_content"] = cached
	deps.Logger.Debug("gemini: injected cached thought_signature", "tool_call_id", tcID)
	return true
}

// geminiExtractFunctionName extracts the function name from a tool call map safely.
// Returns empty string if the function field is missing or not a map.
func geminiExtractFunctionName(tc map[string]interface{}) string {
	fn, ok := tc["function"].(map[string]interface{})
	if !ok {
		return ""
	}
	fnName, _ := fn["name"].(string)
	return fnName
}

func geminiIdentifyOrphanedIDs(deps *SanitizeDeps, toolCallMap map[string]*toolCallInfo) map[string]bool {
	orphanedIDs := make(map[string]bool)
	for tcID, info := range toolCallMap {
		if !info.hasExtra {
			orphanedIDs[tcID] = true
			deps.Logger.Warn("gemini: tool_call missing thought_signature, will strip pair",
				"tool_call_id", tcID)
		}
	}
	return orphanedIDs
}

func geminiInjectFunctionNames(deps *SanitizeDeps, messages []interface{}, toolCallMap map[string]*toolCallInfo) {
	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "tool" {
			continue
		}

		toolCallID, _ := msg["tool_call_id"].(string)
		if toolCallID == "" {
			continue
		}

		if _, hasName := msg["name"]; hasName {
			continue
		}

		info, ok := toolCallMap[toolCallID]
		if !ok || info.name == "" {
			continue
		}

		msg["name"] = info.name
		deps.Logger.Debug("gemini: injected function name on tool message", "tool_call_id", toolCallID, "name", info.name)
	}
}

// geminiSystemMarker prefixes demoted mid-session system messages so the
// model can tell provider-directed notices from genuine user input.
const geminiSystemMarker = "[system]"

// demoteGeminiMidSessionSystem implements the Gemini token-0 prefix
// immutability rule (NENYA-44): only the leading system run may act as the
// cached prefix anchor. Gemini's OpenAI-compat endpoint merges scattered
// system messages unpredictably, so any system message arriving after the
// first user/assistant turn is demoted to a user turn carrying a clear
// role marker.
//
// Demotion never splits an open function-call sequence: notices that
// arrive between an assistant tool_calls message and its tool results are
// buffered and flushed before the next non-tool turn (or at the end of
// the conversation), keeping the tool run contiguous for the upstream.
//
// Returns true when any message was demoted or reordered.
func demoteGeminiMidSessionSystem(deps *SanitizeDeps, messages []interface{}) bool {
	// Locate the end of the leading contiguous system run.
	head := 0
	for head < len(messages) {
		msg, ok := messages[head].(map[string]interface{})
		if !ok {
			break
		}
		if role, _ := msg["role"].(string); role != "system" {
			break
		}
		head++
	}
	if head == len(messages) {
		return false
	}

	changed := false
	var pending []interface{} // demoted notices held during a tool run
	inToolRun := false        // an assistant tool_calls run awaits its results
	demoted := func(msg map[string]interface{}) interface{} {
		demoteMessageToUser(msg)
		return msg
	}

	result := make([]interface{}, 0, len(messages))
	result = append(result, messages[:head]...)

	for i := head; i < len(messages); i++ {
		msgRaw := messages[i]
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			flushGeminiPending(&pending, &inToolRun, &result, demoted)
			result = append(result, msgRaw)
			continue
		}
		role, _ := msg["role"].(string)

		switch role {
		case "system":
			changed = true
			if inToolRun {
				pending = append(pending, msg)
				continue
			}
			result = append(result, demoted(msg))
		case "tool":
			inToolRun = true
			result = append(result, msgRaw)
		case "assistant":
			if _, hasToolCalls := msg["tool_calls"]; !hasToolCalls {
				flushGeminiPending(&pending, &inToolRun, &result, demoted)
			}
			inToolRun = false
			result = append(result, msgRaw)
		default:
			// user or other role: notices flush before it.
			flushGeminiPending(&pending, &inToolRun, &result, demoted)
			inToolRun = false
			result = append(result, msgRaw)
		}
	}
	flushGeminiPending(&pending, &inToolRun, &result, demoted)

	if changed {
		copy(messages, result)
		if tail := len(messages) - len(result); tail > 0 {
			clear(messages[len(result):]) // nil out drained tail slots
		}
		deps.Logger.Debug("demoted mid-session system messages for gemini prefix stability", "count", len(messages)-countGeminiLeadingSystem(result))
	}
	return changed
}

// flushGeminiPending emits buffered demoted notices once the open tool run
// ends, demoting them as it goes.
func flushGeminiPending(pending *[]interface{}, inToolRun *bool, result *[]interface{}, demoted func(map[string]interface{}) interface{}) {
	for _, m := range *pending {
		msg, _ := m.(map[string]interface{})
		*result = append(*result, demoted(msg))
	}
	*pending = (*pending)[:0]
	*inToolRun = false
}

// demoteMessageToUser converts a system message in place to a user turn
// with a clear role marker: string content gains a "[system] " prefix;
// block-array content gains a marker text block ahead of the verbatim
// parts. Any other shape is wrapped as the marker alone.
func demoteMessageToUser(msg map[string]interface{}) {
	msg["role"] = "user"
	switch content := msg["content"].(type) {
	case string:
		msg["content"] = geminiSystemMarker + " " + content
	case []interface{}:
		blocks := make([]interface{}, 0, len(content)+1)
		blocks = append(blocks, map[string]interface{}{
			"type": "text",
			"text": geminiSystemMarker,
		})
		blocks = append(blocks, content...)
		msg["content"] = blocks
	default:
		msg["content"] = geminiSystemMarker
	}
}

// countGeminiLeadingSystem counts the contiguous system run at the head of
// the message slice (used for debug logging only).
func countGeminiLeadingSystem(messages []interface{}) int {
	n := 0
	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			break
		}
		if role, _ := msg["role"].(string); role != "system" {
			break
		}
		n++
	}
	return n
}

func geminiFilterMessages(deps *SanitizeDeps, messages []interface{}, toolCallMap map[string]*toolCallInfo, orphanedIDs map[string]bool) []interface{} {
	filtered := make([]interface{}, 0, len(messages))

	for i, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			filtered = append(filtered, msgRaw)
			continue
		}
		role, _ := msg["role"].(string)

		if role == "assistant" {
			if !geminiFilterAssistantMessage(deps, msg, orphanedIDs, i) {
				filtered = append(filtered, msgRaw)
			}
			continue
		}

		if role == "tool" {
			if !geminiFilterToolMessage(deps, msg, toolCallMap, orphanedIDs) {
				filtered = append(filtered, msgRaw)
			}
			continue
		}

		filtered = append(filtered, msgRaw)
	}

	return filtered
}

func geminiFilterAssistantMessage(deps *SanitizeDeps, msg map[string]interface{}, orphanedIDs map[string]bool, index int) bool {
	toolCallsRaw, hasTC := msg["tool_calls"]
	if !hasTC {
		return false
	}

	toolCalls, ok := toolCallsRaw.([]interface{})
	if !ok {
		return false
	}

	cleaned := geminiCleanToolCalls(toolCalls, orphanedIDs)
	if len(cleaned) == 0 {
		content := deps.ExtractContentText(msg)
		if content == "" {
			deps.Logger.Debug("gemini: removed empty assistant message after stripping orphaned tool_calls", "index", index)
			return true
		}
		delete(msg, "tool_calls")
		return false
	}

	msg["tool_calls"] = cleaned
	return false
}

func geminiCleanToolCalls(toolCalls []interface{}, orphanedIDs map[string]bool) []interface{} {
	cleaned := make([]interface{}, 0, len(toolCalls))
	for _, tcRaw := range toolCalls {
		tc, ok := tcRaw.(map[string]interface{})
		if !ok {
			cleaned = append(cleaned, tcRaw)
			continue
		}
		tcID, _ := tc["id"].(string)
		if orphanedIDs[tcID] {
			continue
		}
		cleaned = append(cleaned, tcRaw)
	}
	return cleaned
}

func geminiFilterToolMessage(deps *SanitizeDeps, msg map[string]interface{}, toolCallMap map[string]*toolCallInfo, orphanedIDs map[string]bool) bool {
	toolCallID, _ := msg["tool_call_id"].(string)
	if toolCallID == "" {
		return false
	}

	if orphanedIDs[toolCallID] {
		deps.Logger.Debug("gemini: removed orphaned tool response", "tool_call_id", toolCallID)
		return true
	}

	if _, hasName := msg["name"]; hasName {
		return false
	}

	info, ok := toolCallMap[toolCallID]
	if ok && info.name != "" {
		msg["name"] = info.name
		deps.Logger.Debug("gemini: injected function name on tool message",
			"tool_call_id", toolCallID, "name", info.name)
	} else {
		msg["name"] = "unknown_function"
		deps.Logger.Warn("gemini: assigned synthetic name to tool message",
			"tool_call_id", toolCallID)
	}

	return false
}

func geminiValidationEndpoint(providerURL string) string {
	u, err := url.Parse(providerURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Host)
	path := u.Path

	if strings.Contains(host, "generativelanguage.googleapis.com") {
		if idx := strings.Index(path, "/openai/chat/completions"); idx != -1 {
			return strings.TrimSuffix(providerURL, "/openai/chat/completions") + "/models"
		}
	}
	return defaultValidationEndpoint(providerURL, path)
}
