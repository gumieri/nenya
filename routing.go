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
