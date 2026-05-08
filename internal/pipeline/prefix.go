package pipeline

import (
	"sort"

	"nenya/config"
)

func ApplyPrefixCacheOptimizations(payload map[string]interface{}, messages []interface{}, cfg config.PrefixCacheConfig) {
	if !cfg.Enabled {
		return
	}

	if cfg.PinSystemFirst != nil && *cfg.PinSystemFirst {
		PinSystemMessages(messages)
	}

	if cfg.StableTools != nil && *cfg.StableTools {
		StabilizeTools(payload)
	}
}

func PinSystemMessages(messages []interface{}) bool {
	if len(messages) < 2 {
		return false
	}

	var systemMsgs []interface{}
	var nonSystemMsgs []interface{}
	anyMoved := false

	for _, msg := range messages {
		msgNode, ok := msg.(map[string]interface{})
		if !ok {
			nonSystemMsgs = append(nonSystemMsgs, msg)
			continue
		}
		role, _ := msgNode["role"].(string)
		if role == "system" {
			systemMsgs = append(systemMsgs, msg)
			if len(nonSystemMsgs) > 0 {
				anyMoved = true
			}
		} else {
			nonSystemMsgs = append(nonSystemMsgs, msg)
		}
	}

	if !anyMoved {
		return false
	}

	pinned := make([]interface{}, 0, len(messages))
	pinned = append(pinned, systemMsgs...)
	pinned = append(pinned, nonSystemMsgs...)

	for i := range messages {
		messages[i] = pinned[i]
	}

	return true
}

func StabilizeTools(payload map[string]interface{}) bool {
	toolsRaw, ok := payload["tools"]
	if !ok {
		return false
	}
	tools, ok := toolsRaw.([]interface{})
	if !ok || len(tools) < 2 {
		return false
	}

	sorted := make([]interface{}, len(tools))
	copy(sorted, tools)
	sort.Slice(sorted, func(i, j int) bool {
		return ToolSortKey(sorted[i]) < ToolSortKey(sorted[j])
	})
	payload["tools"] = sorted

	return true
}

func ToolSortKey(tool interface{}) string {
	t, ok := tool.(map[string]interface{})
	if !ok {
		return ""
	}
	fn, ok := t["function"].(map[string]interface{})
	if !ok {
		return ""
	}
	name, ok := fn["name"].(string)
	if !ok {
		return ""
	}
	return name
}

func ShouldSkipRedaction(msgNode map[string]interface{}, cfg config.PrefixCacheConfig) bool {
	if cfg.Enabled && cfg.SkipRedactionOnSystem != nil && *cfg.SkipRedactionOnSystem {
		role, _ := msgNode["role"].(string)
		if role == "system" {
			return true
		}
	}
	return false
}
