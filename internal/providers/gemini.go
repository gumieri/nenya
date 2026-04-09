package providers

import (
	"bytes"
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
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  true,
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

func (t *GeminiTransformer) TransformSSEChunk(data []byte) ([]byte, error) {
	if len(data) == 0 || !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return data, nil
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return data, nil
	}

	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if delta, ok := choice["delta"].(map[string]interface{}); ok {
				if toolCalls, ok := delta["tool_calls"].([]interface{}); ok {
					for i, tc := range toolCalls {
						if tcMap, ok := tc.(map[string]interface{}); ok {
							if _, exists := tcMap["index"]; !exists {
								tcMap["index"] = i
							}
							if t.OnExtraContent != nil {
								if tcID, _ := tcMap["id"].(string); tcID != "" {
									if extra, hasExtra := tcMap["extra_content"]; hasExtra {
										t.OnExtraContent(tcID, extra)
									}
								}
							}
						}
					}
				}
			}
		}
	}

	transformed, err := json.Marshal(chunk)
	if err != nil {
		return data, fmt.Errorf("failed to marshal transformed chunk: %v", err)
	}

	return transformed, nil
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

	type toolCallInfo struct {
		id       string
		name     string
		hasExtra bool
	}

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
			_, hasExtra := tc["extra_content"]
			if !hasExtra {
				if deps.ThoughtSigCache != nil {
					if cached, found := deps.ThoughtSigCache.Load(tcID); found {
						tc["extra_content"] = cached
						deps.Logger.Debug("gemini: injected cached thought_signature", "tool_call_id", tcID)
						hasExtra = true
					}
				}
			}
			var fnName string
			if fn, ok := tc["function"].(map[string]interface{}); ok {
				fnName, _ = fn["name"].(string)
			}
			toolCallMap[tcID] = &toolCallInfo{
				id:       tcID,
				name:     fnName,
				hasExtra: hasExtra,
			}
		}
	}

	orphanedIDs := make(map[string]bool)
	for tcID, info := range toolCallMap {
		if !info.hasExtra {
			orphanedIDs[tcID] = true
			deps.Logger.Warn("gemini: tool_call missing thought_signature, will strip pair",
				"tool_call_id", tcID)
		}
	}

	if len(orphanedIDs) == 0 {
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
				deps.Logger.Debug("gemini: injected function name on tool message", "tool_call_id", toolCallID, "name", info.name)
			}
		}
		return
	}

	filtered := make([]interface{}, 0, len(messages))
	for i, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			filtered = append(filtered, msgRaw)
			continue
		}
		role, _ := msg["role"].(string)

		if role == "assistant" {
			toolCallsRaw, hasTC := msg["tool_calls"]
			if hasTC {
				toolCalls, ok := toolCallsRaw.([]interface{})
				if ok {
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
					if len(cleaned) == 0 {
						content := deps.ExtractContentText(msg)
						if content == "" {
							deps.Logger.Debug("gemini: removed empty assistant message after stripping orphaned tool_calls", "index", i)
							continue
						}
						delete(msg, "tool_calls")
					} else {
						msg["tool_calls"] = cleaned
					}
				}
			}
			filtered = append(filtered, msgRaw)
			continue
		}

		if role == "tool" {
			toolCallID, _ := msg["tool_call_id"].(string)
			if toolCallID != "" && orphanedIDs[toolCallID] {
				deps.Logger.Debug("gemini: removed orphaned tool response", "tool_call_id", toolCallID)
				continue
			}

			if _, hasName := msg["name"]; !hasName {
				if info, ok := toolCallMap[toolCallID]; ok && info.name != "" {
					msg["name"] = info.name
					deps.Logger.Debug("gemini: injected function name on tool message",
						"tool_call_id", toolCallID, "name", info.name)
				} else {
					msg["name"] = "unknown_function"
					deps.Logger.Warn("gemini: assigned synthetic name to tool message",
						"tool_call_id", toolCallID)
				}
			}

			filtered = append(filtered, msgRaw)
			continue
		}

		filtered = append(filtered, msgRaw)
	}

	if len(filtered) != len(messages) {
		payload["messages"] = filtered
	}
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
