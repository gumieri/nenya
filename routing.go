package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const defaultAgentCooldownSec = 60

var geminiModelMap = map[string]string{
	"gemini-3-flash": "gemini-3-flash-preview",
	"gemini-3-pro":   "gemini-3-pro-preview",

	"gemini-3.1-flash":      "gemini-3.1-flash-preview",
	"gemini-3.1-flash-lite": "gemini-3.1-flash-lite-preview",
	"gemini-3.1-pro":        "gemini-3.1-pro-preview",

	"gemini-flash":      "gemini-2.5-flash",
	"gemini-flash-lite": "gemini-2.5-flash-lite",
	"gemini-pro":        "gemini-2.5-pro",
}

type upstreamTarget struct {
	url      string
	model    string
	coolKey  string
	provider string
}

func (g *NenyaGateway) resolveProvider(modelName string) *Provider {
	if entry, ok := ModelRegistry[modelName]; ok {
		if p, ok := g.providers[entry.Provider]; ok {
			return p
		}
	}
	lower := strings.ToLower(modelName)
	for _, p := range g.providers {
		for _, prefix := range p.RoutePrefixes {
			if strings.HasPrefix(lower, prefix) {
				return p
			}
		}
	}
	return nil
}

func (g *NenyaGateway) determineUpstream(modelName string) string {
	if p := g.resolveProvider(modelName); p != nil {
		return p.URL
	}
	if defaultP, ok := g.providers["zai"]; ok {
		return defaultP.URL
	}
	return ""
}

func (g *NenyaGateway) providerURL(provider, agentURL string) string {
	if agentURL != "" {
		return agentURL
	}
	if p, ok := g.providers[provider]; ok {
		return p.URL
	}
	return ""
}

func (g *NenyaGateway) buildTargetList(agentName string, agent AgentConfig, tokenCount int) []upstreamTarget {
	n := len(agent.Models)
	if n == 0 {
		return nil
	}

	g.agentMu.Lock()
	var start int
	if strings.ToLower(agent.Strategy) != "fallback" {
		start = int(g.agentCounters[agentName]) % n
		g.agentCounters[agentName]++
	}
	now := time.Now()
	cooldowns := make(map[string]time.Time, len(g.modelCooldowns))
	for k, v := range g.modelCooldowns {
		cooldowns[k] = v
	}
	g.agentMu.Unlock()

	active := make([]upstreamTarget, 0, n)
	cooling := make([]upstreamTarget, 0, n)

	for i := 0; i < n; i++ {
		m := agent.Models[(start+i)%n]

		if m.MaxContext > 0 && tokenCount > m.MaxContext {
			g.logger.Info("skipping model: exceeds max_context",
				"model", m.Model, "max_context", m.MaxContext, "tokens", tokenCount)
			continue
		}

		p := g.providerURL(m.Provider, m.URL)
		if p == "" {
			g.logger.Warn("unknown provider, skipping model", "provider", m.Provider, "model", m.Model)
			continue
		}

		t := upstreamTarget{
			url:      p,
			model:    m.Model,
			coolKey:  agentName + ":" + m.Provider + ":" + m.Model,
			provider: m.Provider,
		}
		if now.Before(cooldowns[t.coolKey]) {
			cooling = append(cooling, t)
		} else {
			active = append(active, t)
		}
	}
	return append(active, cooling...)
}

func (g *NenyaGateway) resolveWindowMaxContext(modelName string, targets []upstreamTarget) int {
	if agent, ok := g.config.Agents[modelName]; ok {
		for _, m := range agent.Models {
			if m.MaxContext > 0 {
				return m.MaxContext
			}
		}
	}
	for _, t := range targets {
		if provider, ok := g.providers[t.provider]; ok {
			for _, prefix := range provider.RoutePrefixes {
				if strings.HasPrefix(strings.ToLower(t.model), prefix) {
					return 0
				}
			}
		}
	}
	return 0
}

func (g *NenyaGateway) isGeminiProvider(providerName string) bool {
	if p, ok := g.providers[providerName]; ok {
		return p.AuthStyle == "bearer+x-goog"
	}
	return false
}

func (g *NenyaGateway) sanitizeToolMessagesForGemini(payload map[string]interface{}) {
	messagesRaw, ok := payload["messages"]
	if !ok {
		return
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok {
		return
	}

	type toolCallEntry struct {
		id   string
		name string
	}

	for i, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)

		if role == "assistant" {
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
				if _, hasExtra := tc["extra_content"]; hasExtra {
					continue
				}
				tcID, _ := tc["id"].(string)
				if tcID == "" {
					continue
				}
				if cached, found := g.thoughtSigCache.Load(tcID); found {
					tc["extra_content"] = cached
					g.logger.Debug("gemini: injected cached thought_signature", "tool_call_id", tcID)
				}
			}
			continue
		}

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

		var name string
		for j := i - 1; j >= 0; j-- {
			prevMsg, ok := messages[j].(map[string]interface{})
			if !ok {
				continue
			}
			prevRole, _ := prevMsg["role"].(string)
			if prevRole != "assistant" {
				continue
			}
			toolCallsRaw, ok := prevMsg["tool_calls"]
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
				if id, _ := tc["id"].(string); id == toolCallID {
					if fn, ok := tc["function"].(map[string]interface{}); ok {
						name, _ = fn["name"].(string)
					}
					break
				}
			}
			if name != "" {
				break
			}
		}

		if name != "" {
			msg["name"] = name
			g.logger.Debug("gemini: injected function name on tool message", "tool_call_id", toolCallID, "name", name)
		}
	}
}

func (g *NenyaGateway) isZAIProvider(providerName string) bool {
	if p, ok := g.providers[providerName]; ok {
		return strings.HasPrefix(p.URL, "https://api.z.ai")
	}
	return false
}

func (g *NenyaGateway) sanitizeMessagesForZAI(payload map[string]interface{}) {
	messagesRaw, ok := payload["messages"]
	if !ok {
		return
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok {
		return
	}

	filtered := make([]interface{}, 0, len(messages))

	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := msg["role"].(string)
		content := extractContentText(msg)

		if content == "" && role != "tool" && role != "assistant" {
			continue
		}

		filtered = append(filtered, msgRaw)
	}

	if len(filtered) == 0 {
		return
	}

	merged := make([]interface{}, 0, len(filtered))
	for i, msgRaw := range filtered {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			merged = append(merged, msgRaw)
			continue
		}
		role, _ := msg["role"].(string)

		if i > 0 {
			prevMsg, ok := merged[len(merged)-1].(map[string]interface{})
			if ok {
				prevRole, _ := prevMsg["role"].(string)
				if prevRole == role && role == "user" {
					prevContent := extractContentText(prevMsg)
					currContent := extractContentText(msg)
					prevMsg["content"] = prevContent + "\n\n" + currContent
					continue
				}
			}
		}

		merged = append(merged, msgRaw)
	}

	if len(merged) > 0 {
		if firstMsg, ok := merged[0].(map[string]interface{}); ok {
			if role, _ := firstMsg["role"].(string); role == "user" {
				bridgeMsg := map[string]interface{}{
					"role":    "system",
					"content": "Continue the conversation.",
				}
				merged = append([]interface{}{bridgeMsg}, merged...)
				g.logger.Debug("zai: prepended system bridge before leading user message")
			}
		}
	}

	if len(merged) != len(messages) {
		g.logger.Debug("zai: sanitized message sequence",
			"messages_before", len(messages), "messages_after", len(merged))
	}

	payload["messages"] = merged
}

func (g *NenyaGateway) transformRequestForUpstream(providerName, upstreamURL string, payload map[string]interface{}, model string) ([]byte, string, error) {
	origModel := payload["model"]

	if model != "" {
		payload["model"] = model
	}

	modelRaw, ok := payload["model"]
	if !ok {
		payload["model"] = origModel
		return nil, "", nil
	}

	modelName, ok := modelRaw.(string)
	if !ok {
		payload["model"] = origModel
		return nil, "", nil
	}

	finalModel := modelName

	if g.isGeminiProvider(providerName) {
		if mapped, ok := geminiModelMap[strings.ToLower(modelName)]; ok {
			finalModel = mapped
		}
		payload["model"] = finalModel
		if finalModel != modelName {
			g.logger.Info("gemini model mapping", "from", modelName, "to", finalModel)
		}
		g.sanitizeToolMessagesForGemini(payload)
	}

	if g.isZAIProvider(providerName) {
		g.sanitizeMessagesForZAI(payload)
	}

	// Inject agent system prompt if configured
	if agentNameRaw, ok := origModel.(string); ok {
		if agent, ok := g.config.Agents[agentNameRaw]; ok {
			if agent.SystemPrompt != "" || agent.SystemPromptFile != "" {
				systemPrompt, err := loadPromptFile(agent.SystemPromptFile, agent.SystemPrompt, "")
				if err != nil {
					g.logger.Warn("failed to load agent system prompt, skipping", "agent", agentNameRaw, "err", err)
				} else if systemPrompt != "" {
					// Inject system message if messages exist
					if messagesRaw, ok := payload["messages"]; ok {
						if messages, ok := messagesRaw.([]interface{}); ok && len(messages) > 0 {
							// Check if first message is already a system message
							injectSystem := true
							if len(messages) > 0 {
								if firstMsg, ok := messages[0].(map[string]interface{}); ok {
									if role, ok := firstMsg["role"].(string); ok && role == "system" {
										injectSystem = false
										g.logger.Debug("agent system prompt skipped: first message already system role", "agent", agentNameRaw)
									}
								}
							}
							if injectSystem {
								systemMsg := map[string]interface{}{
									"role":    "system",
									"content": systemPrompt,
								}
								// Insert at beginning
								newMessages := make([]interface{}, 0, len(messages)+1)
								newMessages = append(newMessages, systemMsg)
								newMessages = append(newMessages, messages...)
								payload["messages"] = newMessages
								g.logger.Info("injected agent system prompt", "agent", agentNameRaw, "provider", providerName)
							}
						}
					}
				}
			}
		}
	}

	if entry, ok := ModelRegistry[finalModel]; ok && entry.MaxOutput > 0 {
		if _, hasMaxTokens := payload["max_tokens"]; !hasMaxTokens {
			payload["max_tokens"] = entry.MaxOutput
		} else if v, ok := payload["max_tokens"].(float64); ok && v > float64(entry.MaxOutput) {
			payload["max_tokens"] = entry.MaxOutput
		}
	}

	newBody, err := json.Marshal(payload)

	if origModel == nil {
		delete(payload, "model")
	} else {
		payload["model"] = origModel
	}

	if err != nil {
		return nil, "", fmt.Errorf("failed to marshal transformed request: %v", err)
	}

	return newBody, finalModel, nil
}

func (g *NenyaGateway) injectAPIKey(providerName string, headers http.Header) error {
	p, ok := g.providers[providerName]
	if !ok {
		return fmt.Errorf("unknown provider: %s", providerName)
	}

	switch p.AuthStyle {
	case "none":
		return nil
	case "bearer":
		if p.APIKey == "" {
			return fmt.Errorf("API key missing for provider %s", providerName)
		}
		headers.Set("Authorization", "Bearer "+p.APIKey)
	case "bearer+x-goog":
		if p.APIKey == "" {
			return fmt.Errorf("API key missing for provider %s", providerName)
		}
		headers.Set("Authorization", "Bearer "+p.APIKey)
		headers.Set("x-goog-api-key", p.APIKey)
	default:
		return fmt.Errorf("unknown auth style %q for provider %s", p.AuthStyle, providerName)
	}
	return nil
}
