package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"nenya/internal/infra"
	"nenya/internal/stream"
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

func geminiSanitize(deps *SanitizeDeps, payload map[string]interface{}) {
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
