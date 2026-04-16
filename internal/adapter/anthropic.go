package adapter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type AnthropicAdapter struct {
	version string
}

func NewAnthropicAdapter() *AnthropicAdapter {
	return &AnthropicAdapter{
		version: "2023-06-01",
	}
}

func (a *AnthropicAdapter) InjectAuth(req *http.Request, apiKey string) error {
	if apiKey == "" {
		return fmt.Errorf("anthropic auth: API key is empty")
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", a.version)
	return nil
}

func (a *AnthropicAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	var openai map[string]interface{}
	if err := json.Unmarshal(body, &openai); err != nil {
		return body, nil
	}

	anthropic := a.convertOpenAIToAnthropic(openai, model, stream)

	out, err := json.Marshal(anthropic)
	if err != nil {
		return body, fmt.Errorf("anthropic: failed to marshal converted request: %w", err)
	}
	return out, nil
}

func (a *AnthropicAdapter) MutateResponse(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	var anthropic map[string]interface{}
	if err := json.Unmarshal(body, &anthropic); err != nil {
		return body, nil
	}

	openai := a.convertAnthropicToOpenAI(anthropic)

	out, err := json.Marshal(openai)
	if err != nil {
		return body, fmt.Errorf("anthropic: failed to marshal converted response: %w", err)
	}
	return out, nil
}

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

func (a *AnthropicAdapter) convertOpenAIToAnthropic(openai map[string]interface{}, model string, stream bool) map[string]interface{} {
	anthropic := map[string]interface{}{
		"model":  model,
		"stream": stream,
		"max_tokens": 8192,
	}

	if v, ok := openai["max_tokens"].(float64); ok && v > 0 {
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

	if msgs, ok := openai["messages"].([]interface{}); ok {
		anthropic["messages"] = a.convertMessages(msgs)
	}

	if tools, ok := openai["tools"].([]interface{}); ok && len(tools) > 0 {
		anthropic["tools"] = a.convertTools(tools)
	}

	if tc, ok := openai["tool_choice"]; ok {
		if s, ok := tc.(string); ok {
			switch s {
			case "auto", "required":
				anthropic["tool_choice"] = map[string]interface{}{"type": s}
			case "none":
				delete(anthropic, "tool_choice")
			}
		} else if m, ok := tc.(map[string]interface{}); ok {
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

	return anthropic
}

func (a *AnthropicAdapter) convertMessages(msgs []interface{}) []interface{} {
	var result []interface{}
	for _, msgRaw := range msgs {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		content := msg["content"]

		switch role {
		case "system":
			if content != nil {
				result = append(result, map[string]interface{}{
					"role":    "user",
					"content": content,
				})
				result = append(result, map[string]interface{}{
					"role":    "assistant",
					"content": "Understood.",
				})
			}
		case "user", "assistant", "tool":
			anthMsg := map[string]interface{}{
				"role": role,
			}
			if role == "tool" {
				anthMsg["role"] = "user"
				toolContent := ""
				if content != nil {
					if s, ok := content.(string); ok {
						toolContent = s
					}
				}
				anthMsg["content"] = []interface{}{
					map[string]interface{}{
						"type":     "tool_result",
						"content": toolContent,
					},
				}
			} else if content != nil {
				anthMsg["content"] = content
			}
			result = append(result, anthMsg)
		}
	}
	return result
}

func (a *AnthropicAdapter) convertTools(tools []interface{}) []interface{} {
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
		params := fn["parameters"]

		anthTool := map[string]interface{}{
			"name":        name,
			"description": desc,
		}
		if params != nil {
			anthTool["input_schema"] = params
		} else {
			anthTool["input_schema"] = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		result = append(result, anthTool)
	}
	return result
}

func (a *AnthropicAdapter) convertAnthropicToOpenAI(anthropic map[string]interface{}) map[string]interface{} {
	openai := map[string]interface{}{
		"id":      "anthropic-" + generateID(),
		"object":  "chat.completion",
		"created": 0,
		"model":   anthropic["model"],
	}

	var choices []interface{}
	choice := map[string]interface{}{
		"index": 0,
	}

	delta := map[string]interface{}{}

	if content, ok := anthropic["content"]; ok {
		switch c := content.(type) {
		case string:
			delta["content"] = c
			choice["finish_reason"] = "stop"
		case []interface{}:
			var textParts []string
			var toolCalls []interface{}
			for _, block := range c {
				bm, ok := block.(map[string]interface{})
				if !ok {
					continue
				}
				bType, _ := bm["type"].(string)
				switch bType {
				case "text":
					if t, ok := bm["text"].(string); ok {
						textParts = append(textParts, t)
					}
				case "tool_use":
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
					toolCalls = append(toolCalls, tc)
				case "tool_result":
					if t, ok := bm["content"].(string); ok {
						delta["content"] = t
					}
				}
			}
			if len(textParts) > 0 {
				delta["content"] = strings.Join(textParts, "")
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

	if stopReason, ok := anthropic["stop_reason"].(string); ok {
		switch stopReason {
		case "end_turn":
			choice["finish_reason"] = "stop"
		case "tool_use":
			choice["finish_reason"] = "tool_calls"
		case "max_tokens":
			choice["finish_reason"] = "length"
		default:
			choice["finish_reason"] = "stop"
		}
	}

	choice["delta"] = delta
	choices = append(choices, choice)
	openai["choices"] = choices

	if usage, ok := anthropic["usage"].(map[string]interface{}); ok {
		openai["usage"] = map[string]interface{}{
			"prompt_tokens":     usage["input_tokens"],
			"completion_tokens": usage["output_tokens"],
			"total_tokens":      addFloat64(usage["input_tokens"], usage["output_tokens"]),
		}
	}

	return openai
}

func addFloat64(a, b interface{}) float64 {
	af, _ := a.(float64)
	bf, _ := b.(float64)
	return af + bf
}

func generateID() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 24)
	for i := range b {
		b[i] = charset[i%len(charset)]
	}
	return string(b)
}
