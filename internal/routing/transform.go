package routing

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"

	"nenya/internal/adapter"
	"nenya/internal/config"
	"nenya/internal/discovery"
	"nenya/internal/infra"
	providerpkg "nenya/internal/providers"
)

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

	a := adapter.ForProviderWithAuth(providerName, p.AuthStyle)
	req := &http.Request{Header: headers}
	return a.InjectAuth(req, p.APIKey)
}

func TransformRequestForUpstream(deps TransformDeps, providerName, upstreamURL string, payload map[string]interface{}, model string, maxOutput int) ([]byte, string, error) {
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

	finalModel := modelName

	if spec, ok := providerpkg.Get(providerName); ok {
		if spec.ModelMap != nil {
			if mapped, ok := spec.ModelMap[strings.ToLower(modelName)]; ok {
				finalModel = mapped
			}
			payload["model"] = finalModel
			if finalModel != modelName {
				deps.Logger.Info("provider model mapping", "provider", providerName, "from", modelName, "to", finalModel)
			}
		}
		if spec.SanitizeRequest != nil {
			spec.SanitizeRequest(&providerpkg.SanitizeDeps{
				Logger:             deps.Logger,
				ThoughtSigCache:    deps.ThoughtSigCache,
				ExtractContentText: deps.ExtractContentText,
			}, payload)
		}
	}

	SanitizePayload(deps, payload, providerName)

	if agentNameRaw, ok := origModel.(string); ok {
		if agent, ok := deps.Config.Agents[agentNameRaw]; ok {
			if agent.SystemPrompt != "" || agent.SystemPromptFile != "" {
				systemPrompt, err := config.LoadPromptFile(agent.SystemPromptFile, agent.SystemPrompt, "")
				if err != nil {
					deps.Logger.Warn("failed to load agent system prompt, skipping", "agent", agentNameRaw, "err", err)
				} else if systemPrompt != "" {
					if messagesRaw, ok := payload["messages"]; ok {
						if messages, ok := messagesRaw.([]interface{}); ok && len(messages) > 0 {
							injectSystem := true
							if len(messages) > 0 {
								if firstMsg, ok := messages[0].(map[string]interface{}); ok {
									if role, ok := firstMsg["role"].(string); ok && role == "system" {
										injectSystem = false
										deps.Logger.Debug("agent system prompt skipped: first message already system role", "agent", agentNameRaw)
									}
								}
							}
							if injectSystem {
								systemMsg := map[string]interface{}{
									"role":    "system",
									"content": systemPrompt,
								}
								var cap int
								if len(messages) > math.MaxInt-1 {
									cap = math.MaxInt
								} else {
									cap = len(messages) + 1
								}
								newMessages := make([]interface{}, 0, cap)
								newMessages = append(newMessages, systemMsg)
								newMessages = append(newMessages, messages...)
								payload["messages"] = newMessages
								deps.Logger.Info("injected agent system prompt", "agent", agentNameRaw, "provider", providerName)
							}
						}
					}
				}
			}
		}
	}

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
	if effectiveMaxOutput > 0 {
		if _, hasMaxTokens := payload["max_tokens"]; !hasMaxTokens {
			payload["max_tokens"] = effectiveMaxOutput
		} else if v, ok := payload["max_tokens"].(float64); ok && v > float64(effectiveMaxOutput) {
			payload["max_tokens"] = effectiveMaxOutput
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
