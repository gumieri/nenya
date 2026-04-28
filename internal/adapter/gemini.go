package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

var GeminiModelMap = map[string]string{
	"gemini-3-flash":        "gemini-3-flash-preview",
	"gemini-3-pro":          "gemini-3-pro-preview",
	"gemini-3.1-flash":      "gemini-3.1-flash-preview",
	"gemini-3.1-flash-lite": "gemini-3.1-flash-lite-preview",
	"gemini-3.1-pro":        "gemini-3.1-pro-preview",
	"gemini-flash":          "gemini-2.5-flash",
	"gemini-flash-lite":     "gemini-2.5-flash-lite",
	"gemini-pro":            "gemini-2.5-pro",
}

var geminiRetryablePatterns = []string{
	"resource_exhausted",
	"the response was blocked",
	"content has no parts",
	"quota exceeded",
}

type GeminiAdapter struct {
	thoughtSigCache ThoughtSigCache
	modelMap        map[string]string
	extractContent  func(msg map[string]interface{}) string
	logger          geminiLogger
}

type ThoughtSigCache interface {
	Load(key string) (interface{}, bool)
	Store(key string, value interface{})
}

type geminiLogger interface {
	Debug(msg string, args ...any)
	Warn(msg string, args ...any)
}

type GeminiAdapterDeps struct {
	ThoughtSigCache ThoughtSigCache
	ExtractContent  func(msg map[string]interface{}) string
	Logger          geminiLogger
	ModelMap        map[string]string
}

func NewGeminiAdapter(deps GeminiAdapterDeps) *GeminiAdapter {
	mm := deps.ModelMap
	if mm == nil {
		mm = GeminiModelMap
	}
	return &GeminiAdapter{
		thoughtSigCache: deps.ThoughtSigCache,
		modelMap:        mm,
		extractContent:  deps.ExtractContent,
		logger:          deps.Logger,
	}
}

func (a *GeminiAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}

	changed := false

	if model != "" && a.modelMap != nil {
		if mapped, ok := a.modelMap[strings.ToLower(model)]; ok {
			payload["model"] = mapped
			changed = true
		}
	}

	if a.thoughtSigCache != nil && a.extractContent != nil {
		if a.geminiSanitize(payload) {
			changed = true
		}
	}

	if _, has := payload["stream_options"]; has {
		delete(payload, "stream_options")
		changed = true
	}

	if !changed {
		return body, nil
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return body, fmt.Errorf("gemini: failed to marshal mutated request: %w", err)
	}
	return out, nil
}

func (a *GeminiAdapter) InjectAuth(req *http.Request, apiKey string) error {
	return (&BearerPlusGoogAuth{}).InjectAuth(req, apiKey)
}

func (a *GeminiAdapter) MutateResponse(body []byte) ([]byte, error) {
	if len(body) == 0 || !bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
		return body, nil
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal(body, &chunk); err != nil {
		return body, nil
	}

	if !a.transformToolCallsInDelta(chunk) {
		return body, nil
	}

	out, err := json.Marshal(chunk)
	if err != nil {
		return body, fmt.Errorf("gemini: failed to marshal mutated response: %w", err)
	}
	return out, nil
}

func (a *GeminiAdapter) transformToolCallsInDelta(chunk map[string]interface{}) bool {
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return false
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return false
	}
	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return false
	}
	toolCalls, ok := delta["tool_calls"].([]interface{})
	if !ok {
		return false
	}
	return a.enrichToolCalls(toolCalls)
}

func (a *GeminiAdapter) enrichToolCalls(toolCalls []interface{}) bool {
	transformed := false
	for i, tc := range toolCalls {
		tcMap, ok := tc.(map[string]interface{})
		if !ok {
			continue
		}
		if _, exists := tcMap["index"]; !exists {
			tcMap["index"] = i
			transformed = true
		}
		if a.storeExtraContent(tcMap) {
			transformed = true
		}
	}
	return transformed
}

func (a *GeminiAdapter) storeExtraContent(tcMap map[string]interface{}) bool {
	if a.thoughtSigCache == nil {
		return false
	}
	tcID, _ := tcMap["id"].(string)
	if tcID == "" {
		return false
	}
	extra, hasExtra := tcMap["extra_content"]
	if !hasExtra {
		return false
	}
	a.thoughtSigCache.Store(tcID, extra)
	return true
}

func (a *GeminiAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	switch statusCode {
	case 429:
		return ErrorRateLimited
	case 500, 502, 503, 504:
		return ErrorRetryable
	case 400, 413, 422:
		if len(body) == 0 {
			return ErrorPermanent
		}
		lower := strings.ToLower(string(body))
		for _, pat := range geminiRetryablePatterns {
			if strings.Contains(lower, pat) {
				return ErrorRetryable
			}
		}
		for _, pat := range commonRetryablePatterns {
			if strings.Contains(lower, pat) {
				return ErrorRetryable
			}
		}
		return ErrorPermanent
	default:
		return ErrorPermanent
	}
}

type toolCallInfo struct {
	id       string
	name     string
	hasExtra bool
}

func (a *GeminiAdapter) geminiSanitize(payload map[string]interface{}) bool {
	messagesRaw, ok := payload["messages"]
	if !ok {
		return false
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok {
		return false
	}

	toolCallMap := a.buildToolCallMap(messages)
	orphanedIDs := a.findOrphanedIDs(toolCallMap)

	if len(orphanedIDs) == 0 {
		a.injectToolMessageNames(messages, toolCallMap)
		return false
	}

	filtered := a.filterOrphanedMessages(messages, orphanedIDs, toolCallMap)
	if len(filtered) == len(messages) {
		return false
	}

	payload["messages"] = filtered
	return true
}

func (a *GeminiAdapter) buildToolCallMap(messages []interface{}) map[string]*toolCallInfo {
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
		a.indexToolCalls(toolCalls, toolCallMap)
	}
	return toolCallMap
}

func (a *GeminiAdapter) indexToolCalls(toolCalls []interface{}, toolCallMap map[string]*toolCallInfo) {
	for _, tcRaw := range toolCalls {
		tc, ok := tcRaw.(map[string]interface{})
		if !ok {
			continue
		}
		tcID, _ := tc["id"].(string)
		if tcID == "" {
			continue
		}
		hasExtra := a.ensureExtraContent(tc, tcID)
		var fnName string
		if fn, ok := tc["function"].(map[string]interface{}); ok {
			fnName, _ = fn["name"].(string)
		}
		toolCallMap[tcID] = &toolCallInfo{id: tcID, name: fnName, hasExtra: hasExtra}
	}
}

func (a *GeminiAdapter) ensureExtraContent(tc map[string]interface{}, tcID string) bool {
	_, hasExtra := tc["extra_content"]
	if !hasExtra && a.thoughtSigCache != nil {
		if cached, found := a.thoughtSigCache.Load(tcID); found {
			tc["extra_content"] = cached
			a.logDebug("gemini: injected cached thought_signature", "tool_call_id", tcID)
			return true
		}
	}
	return hasExtra
}

func (a *GeminiAdapter) findOrphanedIDs(toolCallMap map[string]*toolCallInfo) map[string]bool {
	orphanedIDs := make(map[string]bool)
	for tcID, info := range toolCallMap {
		if !info.hasExtra {
			orphanedIDs[tcID] = true
			a.logWarn("gemini: tool_call missing thought_signature, will strip pair",
				"tool_call_id", tcID)
		}
	}
	return orphanedIDs
}

func (a *GeminiAdapter) injectToolMessageNames(messages []interface{}, toolCallMap map[string]*toolCallInfo) {
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
		if info, ok := toolCallMap[toolCallID]; ok && info.name != "" {
			msg["name"] = info.name
			a.logDebug("gemini: injected function name on tool message", "tool_call_id", toolCallID, "name", info.name)
		}
	}
}

func (a *GeminiAdapter) filterOrphanedMessages(messages []interface{}, orphanedIDs map[string]bool, toolCallMap map[string]*toolCallInfo) []interface{} {
	filtered := make([]interface{}, 0, len(messages))
	for i, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			filtered = append(filtered, msgRaw)
			continue
		}
		role, _ := msg["role"].(string)

		if role == "assistant" {
			a.assistantOrphanedFilter(msg, &filtered, i, orphanedIDs, msgRaw)
			continue
		}

		if role == "tool" {
			a.toolOrphanedFilter(msg, &filtered, orphanedIDs, toolCallMap, msgRaw)
			continue
		}

		filtered = append(filtered, msgRaw)
	}
	return filtered
}

func (a *GeminiAdapter) assistantOrphanedFilter(msg map[string]interface{}, filtered *[]interface{}, i int, orphanedIDs map[string]bool, msgRaw interface{}) {
	toolCallsRaw, hasTC := msg["tool_calls"]
	if !hasTC {
		*filtered = append(*filtered, msgRaw)
		return
	}
	toolCalls, ok := toolCallsRaw.([]interface{})
	if !ok {
		*filtered = append(*filtered, msgRaw)
		return
	}

	cleaned := a.stripOrphanedToolCalls(toolCalls, orphanedIDs)
	if len(cleaned) == 0 {
		a.dropEmptyAssistantAfterStrip(msg, i, filtered, msgRaw)
		return
	}
	msg["tool_calls"] = cleaned
	*filtered = append(*filtered, msgRaw)
}

func (a *GeminiAdapter) dropEmptyAssistantAfterStrip(msg map[string]interface{}, i int, filtered *[]interface{}, msgRaw interface{}) {
	var content string
	if a.extractContent != nil {
		content = a.extractContent(msg)
	}
	if content == "" {
		a.logDebug("gemini: removed empty assistant message after stripping orphaned tool_calls", "index", i)
		return
	}
	delete(msg, "tool_calls")
	*filtered = append(*filtered, msgRaw)
}

func (a *GeminiAdapter) stripOrphanedToolCalls(toolCalls []interface{}, orphanedIDs map[string]bool) []interface{} {
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

func (a *GeminiAdapter) toolOrphanedFilter(msg map[string]interface{}, filtered *[]interface{}, orphanedIDs map[string]bool, toolCallMap map[string]*toolCallInfo, msgRaw interface{}) {
	toolCallID, _ := msg["tool_call_id"].(string)

	if toolCallID != "" && orphanedIDs[toolCallID] {
		a.logDebug("gemini: removed orphaned tool response", "tool_call_id", toolCallID)
		return
	}

	a.ensureToolMessageName(msg, toolCallID, toolCallMap)
	*filtered = append(*filtered, msgRaw)
}

func (a *GeminiAdapter) ensureToolMessageName(msg map[string]interface{}, toolCallID string, toolCallMap map[string]*toolCallInfo) {
	if _, hasName := msg["name"]; hasName {
		return
	}

	if info, ok := toolCallMap[toolCallID]; ok && info.name != "" {
		msg["name"] = info.name
		a.logDebug("gemini: injected function name on tool message",
			"tool_call_id", toolCallID, "name", info.name)
		return
	}

	msg["name"] = "unknown_function"
	a.logWarn("gemini: assigned synthetic name to tool message",
		"tool_call_id", toolCallID)
}

func (a *GeminiAdapter) logDebug(msg string, args ...any) {
	if a.logger != nil {
		a.logger.Debug(msg, args...)
	}
}

func (a *GeminiAdapter) logWarn(msg string, args ...any) {
	if a.logger != nil {
		a.logger.Warn(msg, args...)
	}
}
