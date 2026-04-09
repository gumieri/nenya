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

	transformed := false
	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if delta, ok := choice["delta"].(map[string]interface{}); ok {
				if toolCalls, ok := delta["tool_calls"].([]interface{}); ok {
					for i, tc := range toolCalls {
						if tcMap, ok := tc.(map[string]interface{}); ok {
							if _, exists := tcMap["index"]; !exists {
								tcMap["index"] = i
								transformed = true
							}
							if a.thoughtSigCache != nil {
								if tcID, _ := tcMap["id"].(string); tcID != "" {
									if extra, hasExtra := tcMap["extra_content"]; hasExtra {
										a.thoughtSigCache.Store(tcID, extra)
										transformed = true
									}
								}
							}
						}
					}
				}
			}
		}
	}

	if !transformed {
		return body, nil
	}

	out, err := json.Marshal(chunk)
	if err != nil {
		return body, fmt.Errorf("gemini: failed to marshal mutated response: %w", err)
	}
	return out, nil
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
				if a.thoughtSigCache != nil {
					if cached, found := a.thoughtSigCache.Load(tcID); found {
						tc["extra_content"] = cached
						if a.logger != nil {
							a.logger.Debug("gemini: injected cached thought_signature", "tool_call_id", tcID)
						}
						hasExtra = true
					}
				}
			}
			var fnName string
			if fn, ok := tc["function"].(map[string]interface{}); ok {
				fnName, _ = fn["name"].(string)
			}
			toolCallMap[tcID] = &toolCallInfo{id: tcID, name: fnName, hasExtra: hasExtra}
		}
	}

	orphanedIDs := make(map[string]bool)
	for tcID, info := range toolCallMap {
		if !info.hasExtra {
			orphanedIDs[tcID] = true
			if a.logger != nil {
				a.logger.Warn("gemini: tool_call missing thought_signature, will strip pair",
					"tool_call_id", tcID)
			}
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
				if a.logger != nil {
					a.logger.Debug("gemini: injected function name on tool message", "tool_call_id", toolCallID, "name", info.name)
				}
			}
		}
		return false
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
						var content string
						if a.extractContent != nil {
							content = a.extractContent(msg)
						}
						if content == "" {
							if a.logger != nil {
								a.logger.Debug("gemini: removed empty assistant message after stripping orphaned tool_calls", "index", i)
							}
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
				if a.logger != nil {
					a.logger.Debug("gemini: removed orphaned tool response", "tool_call_id", toolCallID)
				}
				continue
			}

			if _, hasName := msg["name"]; !hasName {
				if info, ok := toolCallMap[toolCallID]; ok && info.name != "" {
					msg["name"] = info.name
					if a.logger != nil {
						a.logger.Debug("gemini: injected function name on tool message",
							"tool_call_id", toolCallID, "name", info.name)
					}
				} else {
					msg["name"] = "unknown_function"
					if a.logger != nil {
						a.logger.Warn("gemini: assigned synthetic name to tool message",
							"tool_call_id", toolCallID)
					}
				}
			}

			filtered = append(filtered, msgRaw)
			continue
		}

		filtered = append(filtered, msgRaw)
	}

	if len(filtered) != len(messages) {
		payload["messages"] = filtered
		return true
	}
	return false
}
