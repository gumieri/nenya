package routing

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"

	"nenya/config"
	"nenya/internal/adapter"
	"nenya/internal/discovery"
	"nenya/internal/infra"
	providerpkg "nenya/internal/providers"
)

// TransformDeps provides the dependencies needed for request payload
// transformation (provider lookup, API key injection, metrics, etc.).
type TransformDeps struct {
	Logger             *slog.Logger
	Providers          map[string]*config.Provider
	Config             *config.Config
	ThoughtSigCache    *infra.ThoughtSignatureCache
	ExtractContentText func(msg map[string]interface{}) string
	Catalog            *discovery.ModelCatalog
}

func InjectAPIKey(providerName string, providers map[string]*config.Provider, headers http.Header) error {
	p, ok := providers[providerName]
	if !ok {
		return fmt.Errorf("unknown provider: %s", providerName)
	}

	if p.AuthStyle != "none" && p.APIKey == "" {
		return fmt.Errorf("provider %s has no API key configured", providerName)
	}

	a := adapter.ForProviderWithAuth(providerName, p.AuthStyle)
	req := &http.Request{Header: headers}
	return a.InjectAuth(req, p.APIKey)
}

func resolveModelMapping(deps TransformDeps, payload map[string]interface{}, providerName, modelName string) string {
	finalModel := modelName
	if spec, ok := providerpkg.Get(providerName); ok && spec.ModelMap != nil {
		if mapped, ok := spec.ModelMap[strings.ToLower(modelName)]; ok {
			finalModel = mapped
		}
		payload["model"] = finalModel
		if finalModel != modelName {
			deps.Logger.Info("provider model mapping", "provider", providerName, "from", modelName, "to", finalModel)
		}
	}
	return finalModel
}

func buildSanitizeDeps(deps TransformDeps) *providerpkg.SanitizeDeps {
	sanitizeDeps := &providerpkg.SanitizeDeps{
		Logger:             deps.Logger,
		ThoughtSigCache:    deps.ThoughtSigCache,
		ExtractContentText: deps.ExtractContentText,
	}
	if deps.Catalog != nil {
		sanitizeDeps.SupportsReasoning = func(model string) bool {
			if dm, ok := deps.Catalog.Lookup(model); ok && dm.Metadata != nil {
				return dm.Metadata.SupportsReasoning
			}
			return false
		}
	}
	if deps.Providers != nil {
		sanitizeDeps.ProviderThinking = func(name string) (bool, bool, bool) {
			p, ok := deps.Providers[name]
			if !ok || p.Thinking == nil {
				return false, false, false
			}
			return p.Thinking.Enabled, p.Thinking.ClearThinking, true
		}
	}
	return sanitizeDeps
}

func applyProviderSanitize(deps TransformDeps, payload map[string]interface{}, providerName string) {
	spec, ok := providerpkg.Get(providerName)
	if !ok {
		return
	}
	if spec.SanitizeRequest == nil {
		return
	}
	sanitizeDeps := buildSanitizeDeps(deps)
	spec.SanitizeRequest(sanitizeDeps, payload)
}

func shouldInjectSystem(firstMsg map[string]interface{}, agent config.AgentConfig) bool {
	if agent.ForceSystemPrompt {
		return true
	}
	role, ok := firstMsg["role"].(string)
	if !ok || role != "system" {
		return true
	}
	return false
}

func safeCapPlusOne(n int) int {
	if n >= math.MaxInt {
		return n
	}
	return n + 1
}

func injectSystemMessage(deps TransformDeps, payload map[string]interface{}, agentNameRaw string, systemPrompt string, agent config.AgentConfig) {
	messagesRaw, ok := payload["messages"]
	if !ok {
		return
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok || len(messages) == 0 {
		return
	}
	injectSystem := true
	if !agent.ForceSystemPrompt {
		if firstMsg, ok := messages[0].(map[string]interface{}); ok {
			injectSystem = shouldInjectSystem(firstMsg, agent)
			if !injectSystem {
				deps.Logger.Debug("agent system prompt skipped: first message already system role", "agent", agentNameRaw)
			}
		}
	}
	if !injectSystem {
		return
	}
	systemMsg := map[string]interface{}{
		"role":    "system",
		"content": systemPrompt,
	}
	capMsg := safeCapPlusOne(len(messages))
	if capMsg < 0 {
		deps.Logger.Warn("message count overflow, truncating", "count", len(messages))
		capMsg = len(messages)
	}
	newMessages := make([]interface{}, 0, capMsg)
	newMessages = append(newMessages, systemMsg)
	newMessages = append(newMessages, messages...)
	payload["messages"] = newMessages
	deps.Logger.Info("injected agent system prompt", "agent", agentNameRaw)
}

func resolveAgentSystemPrompt(deps TransformDeps, payload map[string]interface{}, origModel interface{}, providerName string) {
	agentNameRaw, ok := origModel.(string)
	if !ok {
		return
	}
	agent, ok := deps.Config.Agents[agentNameRaw]
	if !ok {
		return
	}
	if agent.SystemPrompt == "" && agent.SystemPromptFile == "" {
		return
	}
	systemPrompt, err := config.LoadPromptFile(agent.SystemPromptFile, agent.SystemPrompt, "")
	if err != nil {
		deps.Logger.Warn("failed to load agent system prompt, skipping", "agent", agentNameRaw, "err", err)
		return
	}
	if systemPrompt == "" {
		return
	}
	injectSystemMessage(deps, payload, agentNameRaw, systemPrompt, agent)
}

func resolveEffectiveMaxOutput(deps TransformDeps, finalModel string, maxOutput int) int {
	effectiveMaxOutput := 0
	if deps.Catalog != nil {
		if m, ok := deps.Catalog.Lookup(finalModel); ok && m.MaxOutput > 0 {
			effectiveMaxOutput = m.MaxOutput
		}
	}
	if effectiveMaxOutput == 0 {
		if entry, ok := config.ModelRegistry[finalModel]; ok && entry.MaxOutput > 0 {
			effectiveMaxOutput = entry.MaxOutput
		}
	}
	if maxOutput > 0 && (effectiveMaxOutput == 0 || maxOutput < effectiveMaxOutput) {
		effectiveMaxOutput = maxOutput
	}
	return effectiveMaxOutput
}

func applyMaxTokens(payload map[string]interface{}, effectiveMaxOutput int) {
	if effectiveMaxOutput <= 0 {
		return
	}
	if _, hasMaxTokens := payload["max_tokens"]; !hasMaxTokens {
		payload["max_tokens"] = effectiveMaxOutput
		return
	}
	v, ok := payload["max_tokens"].(float64)
	if !ok || v <= float64(effectiveMaxOutput) {
		return
	}
	payload["max_tokens"] = effectiveMaxOutput
}

func restoreOriginalModel(payload map[string]interface{}, origModel interface{}) {
	if origModel == nil {
		delete(payload, "model")
	} else {
		payload["model"] = origModel
	}
}

func TransformRequestForUpstream(deps TransformDeps, providerName, upstreamURL string, payload map[string]interface{}, model string, maxOutput int, format string) ([]byte, string, error) {
	origModel := payload["model"]

	if model != "" {
		payload["model"] = model
	}

	modelRaw, ok := payload["model"]
	if !ok {
		if origModel == nil {
			delete(payload, "model")
		} else {
			payload["model"] = origModel
		}
		return nil, "", nil
	}

	modelName, ok := modelRaw.(string)
	if !ok {
		if origModel == nil {
			delete(payload, "model")
		} else {
			payload["model"] = origModel
		}
		return nil, "", nil
	}

	finalModel := resolveModelMapping(deps, payload, providerName, modelName)
	applyProviderSanitize(deps, payload, providerName)
	SanitizePayload(deps, payload, providerName, modelName)
	resolveAgentSystemPrompt(deps, payload, origModel, providerName)

	effectiveMaxOutput := resolveEffectiveMaxOutput(deps, finalModel, maxOutput)
	applyMaxTokens(payload, effectiveMaxOutput)

	if format == "anthropic" {
		stream := false
		if s, ok := payload["stream"].(bool); ok {
			stream = s
		}
		anthropicAdapter := adapter.GetAnthropicAdapter()
		payload = anthropicAdapter.ConvertOpenAIToAnthropicBody(payload, modelName, stream)
	}

	newBody, err := json.Marshal(payload)
	restoreOriginalModel(payload, origModel)

	if err != nil {
		return nil, "", fmt.Errorf("failed to marshal transformed request: %v", err)
	}

	return newBody, finalModel, nil
}

func CopyHeaders(src, dst http.Header) {
	for k, vv := range src {
		lk := strings.ToLower(k)
		switch lk {
		case "connection", "content-length", "content-encoding", "upgrade",
			"transfer-encoding", "te", "trailers", "proxy-authenticate",
			"proxy-authorization", "keep-alive", "proxy-connection":
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func SliceContains(haystack []int, needle int) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
