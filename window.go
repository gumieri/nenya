package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

const windowSystemPrompt = `You are a conversation summarizer. Summarize the following conversation history into a concise summary.
Preserve: file names, key decisions, error patterns, current task, constraints mentioned.
Omit: verbatim code snippets, verbose outputs, redundant back-and-forth.
Keep the summary under %d characters. Output ONLY the summary, no preamble or explanation.`

func (g *NenyaGateway) applyWindowCompaction(ctx context.Context, payload map[string]interface{}, messages []interface{}, tokenCount int, maxContext int) (bool, error) {
	if !g.config.Window.Enabled {
		return false, nil
	}

	effectiveMax := maxContext
	if effectiveMax == 0 {
		effectiveMax = g.config.Window.MaxContext
	}
	if effectiveMax == 0 {
		return false, nil
	}

	threshold := int(float64(effectiveMax) * g.config.Window.TriggerRatio)
	if threshold == 0 || tokenCount <= threshold {
		return false, nil
	}

	activeMessages := g.config.Window.ActiveMessages
	if activeMessages < 2 {
		activeMessages = 2
	}

	if len(messages) <= activeMessages {
		return false, nil
	}

	splitIdx := len(messages) - activeMessages
	history := messages[:splitIdx]
	active := messages[splitIdx:]

	historyText := g.serializeMessages(history)
	if historyText == "" {
		return false, nil
	}

	beforeTokens := tokenCount

	var summary string

	switch g.config.Window.Mode {
	case "truncate":
		summary = g.truncateHistory(historyText)
	case "summarize", "":
		defaultPrompt := fmt.Sprintf(windowSystemPrompt, g.config.Window.SummaryMaxRunes)
		systemPrompt, err := loadPromptFile(g.config.Window.Engine.SystemPromptFile, g.config.Window.Engine.SystemPrompt, defaultPrompt)
		if err != nil {
			g.logger.Warn("failed to load window summarization prompt, using default", "err", err)
			systemPrompt = defaultPrompt
		}
		summary, err = g.callEngine(ctx, g.config.Window.Engine, systemPrompt, historyText)
		if err != nil {
			g.logger.Warn("window summarization failed, falling back to truncation",
				"err", err)
			summary = g.truncateHistory(historyText)
		}
	default:
		g.logger.Warn("unknown window mode, skipping", "mode", g.config.Window.Mode)
		return false, nil
	}

	if summary == "" {
		return false, nil
	}

	maxRunes := g.config.Window.SummaryMaxRunes
	if maxRunes > 0 && utf8.RuneCountInString(summary) > maxRunes {
		summary = string([]rune(summary)[:maxRunes])
	}

	summaryMsg := map[string]interface{}{
		"role": "system",
		"content": fmt.Sprintf("[Nenya Window Summary (%d messages compacted, was ~%d tokens)]:\n%s",
			len(history), beforeTokens, summary),
	}

	newMessages := make([]interface{}, 0, 1+len(active))
	newMessages = append(newMessages, summaryMsg)
	newMessages = append(newMessages, active...)

	payload["messages"] = newMessages

	afterTokens := g.countRequestTokens(payload)
	g.logger.Info("window compaction applied",
		"mode", g.config.Window.Mode,
		"messages_before", len(history)+len(active),
		"messages_after", 1+len(active),
		"tokens_before", beforeTokens,
		"tokens_after", afterTokens,
		"savings", beforeTokens-afterTokens)

	return true, nil
}

func (g *NenyaGateway) serializeMessages(messages []interface{}) string {
	var sb strings.Builder
	for _, msgRaw := range messages {
		msgNode, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msgNode["role"].(string)
		text := extractContentText(msgNode)
		if text == "" {
			continue
		}
		sb.WriteString(role)
		sb.WriteString(":\n")
		sb.WriteString(text)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

func (g *NenyaGateway) callEngine(ctx context.Context, engine EngineConfig, systemPrompt, prompt string) (string, error) {
	p, ok := g.providers[engine.Provider]
	if !ok {
		return "", fmt.Errorf("engine provider %q not found", engine.Provider)
	}

	apiFormat := p.ApiFormat
	if apiFormat == "" {
		apiFormat = "openai"
	}

	var payload map[string]interface{}
	switch apiFormat {
	case "ollama":
		payload = map[string]interface{}{
			"model":  engine.Model,
			"system": systemPrompt,
			"prompt": prompt,
			"stream": false,
		}
	default:
		payload = map[string]interface{}{
			"model": engine.Model,
			"messages": []map[string]string{
				{"role": "system", "content": systemPrompt},
				{"role": "user", "content": prompt},
			},
			"stream": false,
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal engine payload: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewBuffer(encoded))
	if err != nil {
		return "", fmt.Errorf("failed to create engine request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if err := g.injectAPIKey(engine.Provider, req.Header); err != nil {
		return "", fmt.Errorf("engine auth failed: %v", err)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("engine unreachable: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return "", fmt.Errorf("engine returned status %d: %s", resp.StatusCode, string(body))
	}

	var response map[string]interface{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOllamaResponseBytes)).Decode(&response); err != nil {
		return "", fmt.Errorf("failed to decode engine response: %v", err)
	}

	var output string
	switch apiFormat {
	case "ollama":
		resp, ok := response["response"].(string)
		if !ok {
			return "", fmt.Errorf("engine response missing 'response' field")
		}
		output = resp
	default:
		if choices, ok := response["choices"].([]interface{}); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]interface{}); ok {
				if msg, ok := choice["message"].(map[string]interface{}); ok {
					if content, ok := msg["content"].(string); ok {
						output = content
					}
				}
			}
		}
		if output == "" {
			return "", fmt.Errorf("openai response missing content")
		}
	}
	return output, nil
}

func (g *NenyaGateway) truncateHistory(historyText string) string {
	maxRunes := g.config.Window.SummaryMaxRunes
	if maxRunes <= 0 {
		maxRunes = 4000
	}
	runes := []rune(historyText)
	if len(runes) <= maxRunes {
		return historyText
	}
	keepFirst := int(float64(maxRunes) * 0.3)
	keepLast := int(float64(maxRunes) * 0.7)
	if keepFirst+keepLast > maxRunes {
		keepLast = maxRunes - keepFirst
	}
	if keepLast <= 0 {
		keepLast = 1
	}
	separator := "\n... [NENYA: HISTORY TRUNCATED] ...\n"
	sepRunes := []rune(separator)
	result := make([]rune, 0, keepFirst+len(sepRunes)+keepLast)
	result = append(result, runes[:keepFirst]...)
	result = append(result, sepRunes...)
	result = append(result, runes[len(runes)-keepLast:]...)
	return string(result)
}
