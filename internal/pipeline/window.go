package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"unicode/utf8"

	"nenya/internal/config"
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

func addCap(a, b int) int {
	if b > 0 && a > math.MaxInt-b {
		return math.MaxInt
	}
	return a + b
}

func ApplyWindowCompaction(ctx context.Context, deps WindowDeps, payload map[string]interface{}, messages []interface{}, tokenCount int, windowCfg config.WindowConfig, maxContext int, countRequestTokens func(payload map[string]interface{}) int) (bool, error) {
	if !windowCfg.Enabled {
		return false, nil
	}

	effectiveMax := maxContext
	if effectiveMax == 0 {
		effectiveMax = windowCfg.MaxContext
	}
	if effectiveMax == 0 {
		return false, nil
	}

	threshold := int(float64(effectiveMax) * windowCfg.TriggerRatio)
	if threshold == 0 || tokenCount <= threshold {
		return false, nil
	}

	activeMessages := windowCfg.ActiveMessages
	if activeMessages < 2 {
		activeMessages = 2
	}

	if len(messages) <= activeMessages {
		return false, nil
	}

	splitIdx := len(messages) - activeMessages

	// Never start the active window on a tool-result message. A tool message
	// is semantically bound to the preceding assistant+tool_calls message; if
	// that assistant message were in the compacted history, the tool result
	// would be orphaned and providers would reject the conversation.
	// Walk splitIdx backward until active[0] is not a tool message.
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
		return false, nil
	}

	// Preserve leading system messages verbatim. They carry operator-defined
	// safety instructions and guardrails that must survive compaction intact.
	// Only summarize the non-system history between them and the active window.
	var leadingSystem []interface{}
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

	history := messages[historyStart:splitIdx]
	active := messages[splitIdx:]

	historyText := SerializeMessages(history)
	if historyText == "" {
		return false, nil
	}

	beforeTokens := tokenCount

	var summary string

	switch windowCfg.Mode {
	case "truncate":
		summary = TruncateHistory(historyText, windowCfg.SummaryMaxRunes)
	case "tfidf":
		var query string
		for i := len(active) - 1; i >= 0; i-- {
			if msg, ok := active[i].(map[string]interface{}); ok {
				if role, _ := msg["role"].(string); role == "user" {
					query = ExtractContentText(msg)
					break
				}
			}
		}
		summary = TruncateTFIDFHistory(historyText, windowCfg.SummaryMaxRunes, query)
	case "summarize", "":
		defaultPrompt := fmt.Sprintf(WindowSystemPrompt, windowCfg.SummaryMaxRunes)
		ref := windowCfg.Engine
		systemPrompt, err := config.LoadPromptFile(ref.SystemPromptFile, ref.SystemPrompt, defaultPrompt)
		if err != nil {
			deps.Logger.Warn("failed to load window summarization prompt, using default", "err", err)
			systemPrompt = defaultPrompt
		}
		if len(ref.ResolvedTargets) == 0 {
			deps.Logger.Warn("window engine: no resolved targets, falling back to truncation")
			summary = TruncateHistory(historyText, windowCfg.SummaryMaxRunes)
		} else {
			agentName := ref.AgentName
			if agentName == "" {
				agentName = "inline"
			}
			s, err := CallEngineChain(ctx, deps.Client, deps.OllamaClient,
				ref.ResolvedTargets, deps.Logger, deps.InjectAPIKey,
				"window", agentName, systemPrompt, historyText)
			if err != nil {
				deps.Logger.Warn("window summarization failed, falling back to truncation", "err", err)
				summary = TruncateHistory(historyText, windowCfg.SummaryMaxRunes)
			} else {
				summary = s
			}
		}
	default:
		deps.Logger.Warn("unknown window mode, skipping", "mode", windowCfg.Mode)
		return false, nil
	}

	if summary == "" {
		return false, nil
	}

	maxRunes := windowCfg.SummaryMaxRunes
	if maxRunes > 0 && utf8.RuneCountInString(summary) > maxRunes {
		summary = string([]rune(summary)[:maxRunes])
	}

	summaryMsg := map[string]interface{}{
		"role": "system",
		"content": fmt.Sprintf("[Nenya Window Summary (%d messages compacted, was ~%d tokens)]:\n%s",
			len(history), beforeTokens, summary),
	}

	newMessages := make([]interface{}, 0, addCap(addCap(len(leadingSystem), 2), len(active)))
	// Re-inject preserved operator system messages before the summary so they
	// are never lost during compaction.
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
	capacity := keepFirst + len(sepRunes) + keepLast
	if capacity < 0 || capacity > int(float64(maxRunes)*1.5) {
		capacity = maxRunes
	}
	result := make([]rune, 0, capacity)
	result = append(result, runes[:keepFirst]...)
	result = append(result, sepRunes...)
	result = append(result, runes[len(runes)-keepLast:]...)
	return string(result)
}
