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
	var err error

	switch g.config.Window.Mode {
	case "truncate":
		summary = g.truncateHistory(historyText)
	case "summarize", "":
		prompt := fmt.Sprintf(windowSystemPrompt, g.config.Window.SummaryMaxRunes)
		summary, err = g.callOllama(ctx, prompt, historyText)
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

func (g *NenyaGateway) callOllama(ctx context.Context, systemPrompt, prompt string) (string, error) {
	payload := map[string]interface{}{
		"model":  g.config.Ollama.Model,
		"system": systemPrompt,
		"prompt": prompt,
		"stream": false,
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal ollama payload: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.config.Ollama.URL, bytes.NewBuffer(encoded))
	if err != nil {
		return "", fmt.Errorf("failed to create ollama request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.ollamaClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama unreachable: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return "", fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, string(body))
	}

	var ollamaResp map[string]interface{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOllamaResponseBytes)).Decode(&ollamaResp); err != nil {
		return "", fmt.Errorf("failed to decode ollama response: %v", err)
	}

	response, ok := ollamaResp["response"].(string)
	if !ok {
		return "", fmt.Errorf("ollama response missing 'response' field")
	}

	return response, nil
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
