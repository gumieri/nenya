package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"nenya/internal/config"
	"nenya/internal/gateway"
	"nenya/internal/pipeline"
	"nenya/internal/routing"
)

func (p *Proxy) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, p.GW.Config.Server.MaxBodyBytes)
	defer r.Body.Close()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		p.GW.Logger.Error("failed to read request body", "err", err)
		http.Error(w, "Payload too large or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	if r.Context().Err() != nil {
		return
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		p.GW.Logger.Warn("failed to parse JSON, returning Bad Request")
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	modelName, ok := payload["model"].(string)
	if !ok || modelName == "" {
		p.GW.Logger.Warn("missing or empty model field in request body")
		http.Error(w, `Missing or empty "model" field in request body`, http.StatusBadRequest)
		return
	}
	if len(modelName) > MaxModelNameLength {
		p.GW.Logger.Warn("model name exceeds maximum length", "length", len(modelName))
		http.Error(w, "Model name too long", http.StatusBadRequest)
		return
	}

	tokenCount := p.GW.CountRequestTokens(payload)

	var targets []routing.UpstreamTarget
	var cooldownDuration time.Duration
	var agentName string

	var maxRetries int
	if agent, ok := p.GW.Config.Agents[modelName]; ok {
		agentName = modelName
		secs := agent.CooldownSeconds
		if secs == 0 {
			secs = routing.DefaultAgentCooldownSec
		}
		cooldownDuration = time.Duration(secs) * time.Second
		maxRetries = agent.MaxRetries
		targets = p.GW.AgentState.BuildTargetList(p.GW.Logger, modelName, agent, tokenCount, p.GW.Providers)
		if len(targets) == 0 {
			if len(agent.Models) > 0 {
				p.GW.Logger.Warn("all models excluded by max_context",
					"agent", modelName, "tokens", tokenCount)
				http.Error(w, "Request too large for all configured models in this agent", http.StatusRequestEntityTooLarge)
			} else {
				p.GW.Logger.Error("agent has no models configured", "agent", modelName)
				http.Error(w, "Agent has no models configured", http.StatusInternalServerError)
			}
			return
		}
		strategy := agent.Strategy
		if strategy == "" {
			strategy = "round-robin"
		}
		p.GW.Logger.Info("agent routing",
			"agent", modelName, "strategy", strategy, "models_in_chain", len(targets))
	} else {
		provider := routing.ResolveProvider(modelName, p.GW.Providers)
		if provider == nil {
			p.GW.Logger.Warn("no provider found for model", "model", modelName)
			http.Error(w, "No provider configured for this model", http.StatusBadRequest)
			return
		}
		targets = []routing.UpstreamTarget{{URL: provider.URL, Model: modelName, Provider: provider.Name}}
		p.GW.Logger.Info("model routing", "model", modelName, "upstream", provider.URL)
	}

	if messagesRaw, ok := payload["messages"]; ok {
		if messages, ok := messagesRaw.([]interface{}); ok && len(messages) > 0 {
			windowMaxCtx := routing.ResolveWindowMaxContext(modelName, p.GW.Config.Agents)
			if err := p.applyContentPipeline(r.Context(), payload, tokenCount, windowMaxCtx); err != nil {
				p.GW.Logger.Error("content pipeline failed", "err", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
		} else {
			p.GW.Logger.Warn("messages field is not a non-empty array, skipping Ollama interception")
		}
	}

	p.forwardToUpstream(w, r, targets, payload, cooldownDuration, tokenCount, agentName, maxRetries)
}

func (p *Proxy) applyContentPipeline(ctx context.Context, payload map[string]interface{}, tokenCount int, windowMaxCtx int) error {
	messages, ok := payload["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		return nil
	}

	pipeline.ApplyPrefixCacheOptimizations(payload, messages, p.GW.Config.PrefixCache)

	anyRedacted := false
	for _, msgRaw := range messages {
		msgNode, isMap := msgRaw.(map[string]interface{})
		if !isMap {
			continue
		}
		if pipeline.ShouldSkipRedaction(msgNode, p.GW.Config.PrefixCache) {
			continue
		}
		text := gateway.ExtractContentText(msgNode)
		if text == "" {
			continue
		}
		redacted := pipeline.RedactSecrets(text, p.GW.Config.SecurityFilter.Enabled, p.GW.SecretPatterns, p.GW.Config.SecurityFilter.RedactionLabel)
		if redacted != text {
			msgNode["content"] = redacted
			anyRedacted = true
		}
	}
	if anyRedacted {
		if p.GW.Metrics != nil {
			p.GW.Metrics.RecordRedaction()
		}
	}

	messages = payload["messages"].([]interface{})
	if len(messages) == 0 {
		return nil
	}
	if pipeline.ApplyCompaction(messages, p.GW.Config.Compaction) {
		if p.GW.Metrics != nil {
			p.GW.Metrics.RecordCompaction()
		}
	}

	messages = payload["messages"].([]interface{})
	if len(messages) == 0 {
		return nil
	}
	deps := pipeline.WindowDeps{
		Logger:       p.GW.Logger,
		Client:       p.GW.Client,
		OllamaClient: p.GW.OllamaClient,
		Providers:    p.GW.Providers,
		InjectAPIKey: func(providerName string, headers http.Header) error {
			return routing.InjectAPIKey(providerName, p.GW.Providers, headers)
		},
		CountTokens: p.GW.CountTokens,
	}
	if windowed, err := pipeline.ApplyWindowCompaction(ctx, deps, payload, messages, tokenCount, p.GW.Config.Window, windowMaxCtx, p.GW.CountRequestTokens); err != nil {
		p.GW.Logger.Warn("window compaction failed, proceeding without it", "err", err)
	} else if windowed {
		if p.GW.Metrics != nil {
			p.GW.Metrics.RecordWindow(p.GW.Config.Window.Mode)
		}
	}

	messages = payload["messages"].([]interface{})
	if len(messages) == 0 {
		return nil
	}
	lastMsgRaw := messages[len(messages)-1]
	lastMsgNode, ok := lastMsgRaw.(map[string]interface{})
	if !ok {
		return nil
	}

	textForInterception := gateway.ExtractContentText(lastMsgNode)
	if textForInterception == "" {
		p.GW.Logger.Warn("last message has no text content, skipping interception")
		return nil
	}

	contentRunes := utf8.RuneCountInString(textForInterception)
	softLimit := p.GW.Config.Governance.ContextSoftLimit
	hardLimit := p.GW.Config.Governance.ContextHardLimit

	var processed string
	var needsUpdate bool

	if contentRunes < softLimit {
		p.GW.Logger.Debug("payload within soft limit, passing through",
			"runes", contentRunes, "soft_limit", softLimit)
	} else if contentRunes <= hardLimit {
		p.GW.Logger.Warn("payload exceeds soft limit, sending to Ollama",
			"runes", contentRunes)
		if p.GW.Metrics != nil {
			p.GW.Metrics.RecordInterception("soft_limit")
		}
		summarized, err := p.summarizeWithOllama(ctx, textForInterception)
		if err != nil {
			p.GW.Logger.Error("Ollama summarization failed, proceeding with original", "err", err)
		} else {
			processed = fmt.Sprintf("[Nenya Sanitized via Ollama]:\n%s", summarized)
			needsUpdate = true
		}
	} else {
		p.GW.Logger.Warn("payload exceeds hard limit, truncating before Ollama",
			"runes", contentRunes, "hard_limit", hardLimit)
		if p.GW.Metrics != nil {
			p.GW.Metrics.RecordInterception("hard_limit")
		}
		truncated := pipeline.TruncateMiddleOut(textForInterception, hardLimit, p.GW.Config.Governance)
		summarized, err := p.summarizeWithOllama(ctx, truncated)
		if err != nil {
			p.GW.Logger.Error("Ollama summarization failed after truncation, forwarding truncated", "err", err)
			processed = fmt.Sprintf("[Nenya Truncated (Ollama unreachable)]:\n%s", truncated)
		} else {
			processed = fmt.Sprintf("[Nenya Sanitized via Ollama (truncated input)]:\n%s", summarized)
		}
		needsUpdate = true
	}

	if needsUpdate {
		lastMsgNode["content"] = processed
	}

	return nil
}

func (p *Proxy) summarizeWithOllama(ctx context.Context, heavyText string) (string, error) {
	engine := p.GW.Config.SecurityFilter.Engine
	ctx, cancel := context.WithTimeout(ctx, time.Duration(engine.TimeoutSeconds)*time.Second)
	defer cancel()

	defaultPrompt := "You are a data privacy filter. Review the following text and remove or replace any IP addresses, AWS keys (AKIA...), passwords, tokens, or credentials with [REDACTED]. Preserve the original structure, detail level, and all non-sensitive content exactly as provided. Do NOT summarize or shorten the text."
	systemPrompt, err := config.LoadPromptFile(engine.SystemPromptFile, engine.SystemPrompt, defaultPrompt)
	if err != nil {
		p.GW.Logger.Warn("failed to load privacy filter prompt, using default", "err", err)
		systemPrompt = defaultPrompt
	}

	provider, ok := p.GW.Providers[engine.Provider]
	if !ok {
		return "", fmt.Errorf("engine provider %q not found", engine.Provider)
	}
	httpClient := p.GW.Client
	if provider.ApiFormat == "ollama" {
		httpClient = p.GW.OllamaClient
	}

	summary, err := pipeline.CallEngine(ctx, httpClient, provider, engine, func(providerName string, headers http.Header) error {
		return routing.InjectAPIKey(providerName, p.GW.Providers, headers)
	}, systemPrompt, heavyText)
	if err != nil {
		return "", err
	}
	return summary, nil
}

func (p *Proxy) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, p.GW.Config.Server.MaxBodyBytes)
	defer r.Body.Close()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		p.GW.Logger.Error("failed to read embeddings request body", "err", err)
		http.Error(w, "Payload too large or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	var payload map[string]interface{}
	if err = json.Unmarshal(bodyBytes, &payload); err != nil {
		p.GW.Logger.Warn("failed to parse embeddings JSON")
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	modelName, ok := payload["model"].(string)
	if !ok || modelName == "" {
		p.GW.Logger.Warn("missing or empty model in embeddings request")
		http.Error(w, `Missing or empty "model" field`, http.StatusBadRequest)
		return
	}
	if len(modelName) > MaxModelNameLength {
		p.GW.Logger.Warn("model name exceeds maximum length in embeddings request", "length", len(modelName))
		http.Error(w, "Model name too long", http.StatusBadRequest)
		return
	}

	provider := routing.ResolveProvider(modelName, p.GW.Providers)
	if provider == nil {
		p.GW.Logger.Warn("no provider for embeddings model", "model", modelName)
		http.Error(w, "No provider configured for this model", http.StatusBadRequest)
		return
	}

	embeddingURL := strings.TrimSuffix(provider.URL, "/chat/completions") + "/embeddings"
	if embeddingURL == provider.URL {
		p.GW.Logger.Warn("provider URL does not end with /chat/completions, cannot derive embeddings endpoint",
			"provider", provider.Name, "url", provider.URL)
		http.Error(w, "Provider does not support embeddings", http.StatusBadRequest)
		return
	}

	req, err := p.buildUpstreamRequest(r.Context(), http.MethodPost, embeddingURL, bodyBytes, provider.Name, r.Header)
	if err != nil {
		p.GW.Logger.Error("failed to create embeddings upstream request", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.GW.Client.Do(req)
	if err != nil {
		p.GW.Logger.Error("embeddings upstream request failed", "provider", provider.Name, "err", err)
		http.Error(w, "Upstream provider error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	routing.CopyHeaders(resp.Header, w.Header())
	w.WriteHeader(resp.StatusCode)

	if _, err := io.Copy(w, resp.Body); err != nil {
		p.GW.Logger.Debug("embeddings response copy ended", "err", err)
	}
}

func (p *Proxy) buildUpstreamRequest(ctx context.Context, method, url string, body []byte, providerName string, srcHeaders http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create upstream request: %w", err)
	}
	headers := srcHeaders.Clone()
	headers.Del("Authorization")
	if err := routing.InjectAPIKey(providerName, p.GW.Providers, headers); err != nil {
		return nil, fmt.Errorf("API key injection failed: %w", err)
	}
	routing.CopyHeaders(headers, req.Header)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", p.GW.Config.Server.UserAgent)
	return req, nil
}
