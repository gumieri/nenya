package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/routing"
)

// summarizeMessages compresses the messages array using the configured engine chain.
// It serializes all messages, sends them to the summarization engine, and returns
// a replacement message array with a single assistant message containing the summary.
// Only one summarization attempt is allowed per request to avoid loops.
func (p *Proxy) summarizeMessages(ctx context.Context, gw *gateway.NenyaGateway, messages []interface{}, agentName, providerName, modelName string) ([]interface{}, error) {
	if len(gw.Config.Bouncer.Engine.ResolvedTargets) == 0 {
		return nil, fmt.Errorf("engine chain not configured")
	}

	var textForSummary strings.Builder
	for _, msgRaw := range messages {
		if msgMap, ok := msgRaw.(map[string]interface{}); ok {
			fmt.Fprintf(&textForSummary, "%s: %s\n", msgMap["role"], gateway.ExtractContentText(msgMap))
		}
	}

	if textForSummary.Len() == 0 {
		return nil, fmt.Errorf("no text content to summarize")
	}

	start := time.Now()
	summary, err := pipeline.CallEngineChain(
		ctx, gw.ClientFor,
		gw.Config.Bouncer.Engine.ResolvedTargets, gw.Logger,
		func(providerName string, headers http.Header) error {
			return routing.InjectAPIKeyWithGateway(providerName, gw, headers)
		},
		"context_limit_retry", gw.Config.Bouncer.Engine.AgentName, retrySystemPrompt, textForSummary.String())

	if gw.Metrics != nil {
		gw.Metrics.RecordSummarizationDuration(agentName, providerName, modelName, time.Since(start))
	}

	if err != nil {
		return nil, fmt.Errorf("summarization failed: %w", err)
	}

	return []interface{}{
		map[string]interface{}{
			"role":    "assistant",
			"content": summary,
		},
	}, nil
}

// attemptContextLimitSummarization attempts to summarize the request messages
// after a context-length error. It parses the original payload, extracts messages,
// sends them to the configured summarization engine with the provided context,
// and returns a summarized payload map on success.
func (p *Proxy) attemptContextLimitSummarization(ctx context.Context, ctxLogger *slog.Logger, gw *gateway.NenyaGateway, originalPayload []byte, errorBody []byte, agentName, providerName, modelName string, canary pipeline.CanarResult) (map[string]interface{}, error) {
	if len(gw.Config.Bouncer.Engine.ResolvedTargets) == 0 {
		return nil, fmt.Errorf("engine chain not configured")
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(originalPayload, &payload); err != nil {
		ctxLogger.Warn("failed to unmarshal original payload for summarization", "err", err)
		return nil, fmt.Errorf("failed to unmarshal original payload: %w", err)
	}

	messagesRaw, ok := payload["messages"]
	if !ok || messagesRaw == nil {
		return nil, fmt.Errorf("no messages in payload")
	}

	messages, ok := messagesRaw.([]interface{})
	if !ok || len(messages) == 0 {
		return nil, fmt.Errorf("messages is not a valid array or is empty")
	}

	// The canary marker must not reach the summarizer: a faithful
	// summary would reproduce the token verbatim and trip the wire on
	// the retried conversation. It is re-appended verbatim below.
	messages = canarySafeMessages(messages, canary)

	summarized, err := p.summarizeMessages(ctx, gw, messages, agentName, providerName, modelName)
	if err != nil {
		return nil, err
	}

	newPayload := make(map[string]interface{}, len(payload))
	for k, v := range payload {
		newPayload[k] = v
	}
	// The canary marker (a trailing system message) is part of the
	// original conversation but is not summarizable content: re-append
	// it verbatim so the tripwire stays armed on the summarized retry.
	if canary.Token != "" {
		summarized = append(summarized, map[string]interface{}{
			"role":    "system",
			"content": pipeline.CanaryMarker(canary.Token),
		})
	}
	newPayload["messages"] = summarized
	return newPayload, nil
}

// canarySafeMessages strips canary material from the summarizer input:
// the injected marker message is removed entirely and a bare token
// echoed into other history content (e.g. log-mode-allowed tool output)
// is scrubbed, so the summary can never reproduce the token.
func canarySafeMessages(messages []interface{}, canary pipeline.CanarResult) []interface{} {
	if canary.Token == "" {
		return messages
	}
	filtered := make([]interface{}, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			filtered = append(filtered, m)
			continue
		}
		if msg["content"] == pipeline.CanaryMarker(canary.Token) {
			continue
		}
		switch text := msg["content"].(type) {
		case string:
			if strings.Contains(text, canary.Token) {
				msg["content"] = strings.ReplaceAll(text, canary.Token, "")
			}
		case []interface{}:
			// Content-array form: scrub bare tokens from text parts.
			for _, part := range text {
				pm, ok := part.(map[string]interface{})
				if !ok || pm["type"] != "text" {
					continue
				}
				if pt, ok := pm["text"].(string); ok && strings.Contains(pt, canary.Token) {
					pm["text"] = strings.ReplaceAll(pt, canary.Token, "")
				}
			}
		}
		filtered = append(filtered, m)
	}
	return filtered
}

// handleContextLimitError processes context-length exceeded errors with optional summarization.
func (rl *retryLoop) handleContextLimitError(i int, target routing.UpstreamTarget, action upstreamAction) retrySignal {
	rl.gw.Metrics.RecordContextLimitError(rl.opts.AgentName, target.Provider, target.Model)
	rl.ctxLogger.Warn("context length exceeded error from upstream",
		"status", action.resp.StatusCode,
		"provider", target.Provider,
		"model", target.Model)

	if !rl.gw.Config.Governance.AutoRetryOnContextLimitEnabled() {
		rl.ctxLogger.Info("auto_retry_on_context_limit disabled")
	} else if !rl.summarized {
		summarizedPayload, sumErr := rl.p.attemptContextLimitSummarization(
			rl.r.Context(), rl.ctxLogger, rl.gw, rl.originalPayload, action.body, rl.opts.AgentName, target.Provider, target.Model, rl.opts.Canary)
		if sumErr == nil && summarizedPayload != nil {
			rl.summarized = true
			rl.summarizedPayload = summarizedPayload
			rl.gw.Metrics.RecordSummarizationRetry(rl.opts.AgentName, target.Provider, target.Model)
			rl.ctxLogger.Info("context limit summarization succeeded, retrying with summarized payload")
			rl.lastFailReason = failReasonSummarized
			// The summarized retry re-dispatches and consumes a fresh probe.
			rl.gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
			return retrySignalRetry
		}
		rl.ctxLogger.Warn("context limit summarization failed", "err", sumErr)
	} else {
		rl.ctxLogger.Warn("already attempted summarization")
	}

	gwErr := ParseProviderError(target.Provider, action.resp.StatusCode, action.body, nil)
	rl.gw.AgentState.CB.ReleaseHalfOpen(target.CoolKey)
	rl.writeUpstreamErrorToClient(action.resp.StatusCode, gwErr)
	return retrySignalDone
}
