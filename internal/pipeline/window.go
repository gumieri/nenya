// Package pipeline implements content processing pipelines for incoming requests,
// including message compaction, secret redaction, and window summarization.
//
// The window compaction feature reduces conversation history size when approaching
// context limits by:
// - Detecting when message count exceeds threshold (configurable trigger ratio)
// - Compacting older messages via summarization, truncation, or TF-IDF relevance filtering
// - Keeping recent messages intact for conversation continuity
//
// Compaction modes (configurable via governance.window.mode):
// - "summarize": Engine-based summarization using Ollama or configured agent
// - "truncate": Simple truncation keeping first N% and last M% of history
// - "tfidf": Relevance scoring against recent user query, keeping only relevant blocks
//
// Window compaction is applied when:
// 1. Total message count > window.max_context * window.trigger_ratio
// 2. At least window.active_messages are preserved (default 2)
// 3. Configured governance.window.enabled == true
package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"nenya/config"
	"nenya/internal/util"
)

const WindowSystemPrompt = `You are a conversation summarizer. Summarize the following conversation history into a concise summary.
Preserve: file names, key decisions, error patterns, current task, constraints mentioned.
Omit: verbatim code snippets, verbose outputs, redundant back-and-forth.
Keep the summary under %d characters. Output ONLY the summary, no preamble or explanation.`

type WindowDeps struct {
	Logger       *slog.Logger
	Client       *http.Client
	OllamaClient *http.Client
	Providers    map[string]*config.Provider
	InjectAPIKey func(providerName string, headers http.Header) error
	CountTokens  func(text string) int
}

func ApplyWindowCompaction(ctx context.Context, deps WindowDeps, payload map[string]interface{}, messages []interface{}, tokenCount int, windowCfg config.WindowConfig, maxContext int, countRequestTokens func(payload map[string]interface{}) int) (bool, error) {
	if !windowCfg.Enabled {
		return false, nil
	}

	_, _, _, active, history, leadingSystem, ok := calculateWindowParams(windowCfg, maxContext, tokenCount, messages)
	if !ok {
		return false, nil
	}

	historyText := SerializeMessages(history)
	if historyText == "" {
		return false, nil
	}

	beforeTokens := tokenCount
	summary, err := generateWindowSummary(ctx, deps, windowCfg, active, historyText)
	if err != nil || summary == "" {
		if err != nil {
			deps.Logger.Warn("window summarization failed, skipping", "err", err)
		}
		return false, nil
	}

	summary = trimSummaryToMaxRunes(summary, windowCfg.SummaryMaxRunes)
	if summary == "" {
		return false, nil
	}

	newMessages := buildCompactedMessages(leadingSystem, active, summary, len(history), beforeTokens)
	payload["messages"] = newMessages

	afterTokens := countRequestTokens(payload)
	deps.Logger.Info("window compaction applied",
		"mode", windowCfg.Mode,
		"messages_before", len(history)+len(active),
		"messages_after", 1+len(active),
		"tokens_before", beforeTokens,
		"tokens_after", afterTokens,
		"savings", beforeTokens-afterTokens)

	return true, nil
}

func calculateWindowParams(windowCfg config.WindowConfig, maxContext int, tokenCount int, messages []interface{}) (effectiveMax int, threshold int, splitIdx int, active []interface{}, history []interface{}, leadingSystem []interface{}, ok bool) {
	effectiveMax = maxContext
	if effectiveMax == 0 {
		effectiveMax = windowCfg.MaxContext
	}
	if effectiveMax == 0 {
		return 0, 0, 0, nil, nil, nil, false
	}

	threshold = int(float64(effectiveMax) * windowCfg.TriggerRatio)
	if threshold == 0 || tokenCount <= threshold {
		return 0, 0, 0, nil, nil, nil, false
	}

	activeMessages := windowCfg.ActiveMessages
	if activeMessages < 2 {
		activeMessages = 2
	}

	if len(messages) <= activeMessages {
		return 0, 0, 0, nil, nil, nil, false
	}

	splitIdx = len(messages) - activeMessages

	for splitIdx > 0 {
		msg, ok := messages[splitIdx].(map[string]interface{})
		if !ok {
			break
		}
		if role, _ := msg["role"].(string); role != "tool" {
			break
		}
		splitIdx--
	}

	if splitIdx <= 0 {
		return 0, 0, 0, nil, nil, nil, false
	}

	historyStart := 0
	for historyStart < splitIdx {
		msg, ok := messages[historyStart].(map[string]interface{})
		if !ok {
			break
		}
		if role, _ := msg["role"].(string); role != "system" {
			break
		}
		leadingSystem = append(leadingSystem, messages[historyStart])
		historyStart++
	}

	history = messages[historyStart:splitIdx]
	active = messages[splitIdx:]

	return effectiveMax, threshold, splitIdx, active, history, leadingSystem, true
}

func generateWindowSummary(ctx context.Context, deps WindowDeps, windowCfg config.WindowConfig, active []interface{}, historyText string) (string, error) {
	switch windowCfg.Mode {
	case "truncate":
		return TruncateHistory(historyText, windowCfg.SummaryMaxRunes), nil
	case "tfidf":
		query := extractQueryFromActiveMessages(active)
		cfg := config.ContextConfig{
			TruncationKeepFirstPct: windowCfg.KeepFirstPct,
			TruncationKeepLastPct:  windowCfg.KeepLastPct,
		}
		return TruncateTFIDFHistory(historyText, windowCfg.SummaryMaxRunes, query, cfg), nil
	case "summarize", "":
		return generateEngineSummary(ctx, deps, windowCfg, active, historyText)
	default:
		deps.Logger.Warn("unknown window mode, skipping", "mode", windowCfg.Mode)
		return "", nil
	}
}

func extractQueryFromActiveMessages(active []interface{}) string {
	for i := len(active) - 1; i >= 0; i-- {
		if msg, ok := active[i].(map[string]interface{}); ok {
			if role, _ := msg["role"].(string); role == "user" {
				return ExtractContentText(msg)
			}
		}
	}
	return ""
}

func generateEngineSummary(ctx context.Context, deps WindowDeps, windowCfg config.WindowConfig, active []interface{}, historyText string) (string, error) {
	defaultPrompt := fmt.Sprintf(WindowSystemPrompt, windowCfg.SummaryMaxRunes)
	ref := windowCfg.Engine
	systemPrompt, err := config.LoadPromptFile(ref.SystemPromptFile, ref.SystemPrompt, defaultPrompt)
	if err != nil {
		deps.Logger.Warn("failed to load window summarization prompt, using default", "err", err)
		systemPrompt = defaultPrompt
	}
	if len(ref.ResolvedTargets) == 0 {
		deps.Logger.Warn("window engine: no resolved targets, falling back to truncation")
		return TruncateHistory(historyText, windowCfg.SummaryMaxRunes), nil
	}
	agentName := ref.AgentName
	if agentName == "" {
		agentName = "inline"
	}
	s, err := CallEngineChain(ctx, deps.Client, deps.OllamaClient,
		ref.ResolvedTargets, deps.Logger, deps.InjectAPIKey,
		"window", agentName, systemPrompt, historyText)
	if err != nil {
		deps.Logger.Warn("window summarization failed, falling back to truncation", "err", err)
		return TruncateHistory(historyText, windowCfg.SummaryMaxRunes), nil
	}
	return s, nil
}

func trimSummaryToMaxRunes(summary string, maxRunes int) string {
	if maxRunes <= 0 {
		return summary
	}
	i := 0
	for pos := range summary {
		if i == maxRunes {
			return summary[:pos]
		}
		i++
	}
	return summary
}

func buildCompactedMessages(leadingSystem []interface{}, active []interface{}, summary string, historyLen int, beforeTokens int) []interface{} {
	summaryMsg := map[string]interface{}{
		"role": "system",
		"content": fmt.Sprintf("[Nenya Window Summary (%d messages compacted, was ~%d tokens)]:\n%s",
			historyLen, beforeTokens, summary),
	}

	newMessages := make([]interface{}, 0, util.AddCap(util.AddCap(len(leadingSystem), 2), len(active)))
	newMessages = append(newMessages, leadingSystem...)
	newMessages = append(newMessages, summaryMsg)

	if len(active) > 0 {
		if firstActive, ok := active[0].(map[string]interface{}); ok {
			if role, _ := firstActive["role"].(string); role == "assistant" {
				newMessages = append(newMessages, map[string]interface{}{
					"role":    "user",
					"content": "[Continuing from compacted conversation. Please proceed with the current task.]",
				})
			}
		}
	}

	newMessages = append(newMessages, active...)
	return newMessages
}

func SerializeMessages(messages []interface{}) string {
	var sb strings.Builder
	for _, msgRaw := range messages {
		msgNode, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msgNode["role"].(string)
		text := ExtractContentText(msgNode)
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

func ExtractContentText(msg map[string]interface{}) string {
	contentRaw, ok := msg["content"]
	if !ok {
		return ""
	}
	switch content := contentRaw.(type) {
	case string:
		return content
	case []interface{}:
		var sb strings.Builder
		for _, partRaw := range content {
			if part, ok := partRaw.(map[string]interface{}); ok {
				if text, ok := part["text"].(string); ok {
					sb.WriteString(text)
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

func TruncateHistory(historyText string, maxRunes int) string {
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
	capacity := util.AddCap(util.AddCap(keepFirst, len(sepRunes)), keepLast)
	if capacity < 0 || capacity > int(float64(maxRunes)*1.5) {
		capacity = maxRunes
	}
	result := make([]rune, 0, capacity)
	result = append(result, runes[:keepFirst]...)
	result = append(result, sepRunes...)
	result = append(result, runes[len(runes)-keepLast:]...)
	return string(result)
}
