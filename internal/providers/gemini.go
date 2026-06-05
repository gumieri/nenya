package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"git.0ur.uk/nenya/internal/infra"
	"git.0ur.uk/nenya/internal/stream"
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
}

func newGeminiTransformer(cache *infra.ThoughtSignatureCache) stream.ResponseTransformer {
	return &GeminiTransformer{
		OnExtraContent: func(toolCallID string, extraContent interface{}) {
			if cache != nil {
				cache.Store(toolCallID, extraContent)
			}
		},
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
	if !ok || len(choices) == 0 {
		return
	}

	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return
	}

	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return
	}

	toolCalls, ok := delta["tool_calls"].([]interface{})
	if !ok {
		return
	}

	for i, tc := range toolCalls {
		tcMap, ok := tc.(map[string]interface{})
		if !ok {
			continue
		}

		if _, exists := tcMap["index"]; !exists {
			tcMap["index"] = i
		}

		t.handleExtraContent(tcMap)
	}
}

func (t *GeminiTransformer) handleExtraContent(tcMap map[string]interface{}) {
	if t.OnExtraContent == nil {
		return
	}

	tcID, _ := tcMap["id"].(string)
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

func geminiSanitize(deps *SanitizeDeps, payload map[string]interface{}) {
	injectThinkingForGemini(deps, payload)
	messagesRaw, ok := payload["messages"]
	if !ok {
		return
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok {
		return
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
