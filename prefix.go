package main

import (
	"sort"
)

func (g *NenyaGateway) applyPrefixCacheOptimizations(payload map[string]interface{}, messages []interface{}) {
	if !g.config.PrefixCache.Enabled {
		return
	}

	mutated := false

	if g.config.PrefixCache.PinSystemFirst {
		if g.pinSystemMessages(messages) {
			mutated = true
		}
	}

	if g.config.PrefixCache.StableTools {
		if g.stabilizeTools(payload) {
			mutated = true
		}
	}

	if mutated {
		g.logger.Debug("prefix cache optimizations applied",
			"pin_system", g.config.PrefixCache.PinSystemFirst,
			"stable_tools", g.config.PrefixCache.StableTools)
	}
}

func (g *NenyaGateway) pinSystemMessages(messages []interface{}) bool {
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

func (g *NenyaGateway) stabilizeTools(payload map[string]interface{}) bool {
	toolsRaw, ok := payload["tools"]
	if !ok {
		return false
	}
	tools, ok := toolsRaw.([]interface{})
	if !ok || len(tools) < 2 {
		return false
	}

	sort.Slice(tools, func(i, j int) bool {
		return toolSortKey(tools[i]) < toolSortKey(tools[j])
	})

	return true
}

func toolSortKey(tool interface{}) string {
	t, ok := tool.(map[string]interface{})
	if !ok {
		return ""
	}
	fn, ok := t["function"].(map[string]interface{})
	if !ok {
		return ""
	}
	name, _ := fn["name"].(string)
	return name
}

func (g *NenyaGateway) shouldSkipRedaction(msgNode map[string]interface{}) bool {
	if g.config.PrefixCache.Enabled && g.config.PrefixCache.SkipRedactionOnSystem {
		role, _ := msgNode["role"].(string)
		if role == "system" {
			return true
		}
	}
	return false
}
