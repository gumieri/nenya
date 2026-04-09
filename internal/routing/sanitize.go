package routing

import (
	"strings"

	"nenya/internal/config"
)

func SanitizePayload(deps TransformDeps, payload map[string]interface{}, providerName string) {
	if _, ok := payload["stream_options"]; ok {
		if !providerSupportsStreamOptions(providerName, deps.Providers) {
			delete(payload, "stream_options")
			deps.Logger.Debug("stripped stream_options for provider", "provider", providerName)
		}
	}

	if toolChoice, ok := payload["tool_choice"]; ok {
		if tc, ok := toolChoice.(string); ok && tc == "auto" {
			if !providerSupportsAutoToolChoice(providerName, deps.Providers) {
				delete(payload, "tool_choice")
				deps.Logger.Debug("stripped tool_choice \"auto\" for provider", "provider", providerName)
			}
		}
	}

	if messagesRaw, ok := payload["messages"]; ok {
		if messages, ok := messagesRaw.([]interface{}); ok {
			if !providerSupportsContentArrays(providerName, deps.Providers) {
				changed := false
				for i, msgRaw := range messages {
					msg, ok := msgRaw.(map[string]interface{})
					if !ok {
						continue
					}
					if contentRaw, ok := msg["content"]; ok {
						if arr, ok := contentRaw.([]interface{}); ok && len(arr) > 0 {
							if flat := flattenContentArray(arr); flat != "" {
								messages[i].(map[string]interface{})["content"] = flat
								changed = true
							}
						}
					}
				}
				if changed {
					deps.Logger.Debug("flattened content arrays for provider", "provider", providerName)
				}
			}
		}
	}
}

func flattenContentArray(arr []interface{}) string {
	var parts []string
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if typ, ok := m["type"].(string); ok && typ == "text" {
			if text, ok := m["text"].(string); ok {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func providerSupportsStreamOptions(providerName string, providers map[string]*config.Provider) bool {
	switch strings.ToLower(providerName) {
	case "deepseek", "zai", "openrouter":
		return true
	}
	return false
}

func providerSupportsAutoToolChoice(providerName string, providers map[string]*config.Provider) bool {
	switch strings.ToLower(providerName) {
	case "gemini", "deepseek", "zai", "openrouter", "openai", "github":
		return true
	}
	return false
}

func providerSupportsContentArrays(providerName string, providers map[string]*config.Provider) bool {
	switch strings.ToLower(providerName) {
	case "gemini", "deepseek", "zai", "openrouter", "openai", "github", "together", "sambanova", "cerebras":
		return true
	}
	return false
}
