package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/routing"
)

// BouncerInterceptor sends content to the engine chain for summarization
// and redaction when the payload exceeds soft/hard token limits.
// Priority: 50 — runs last, after all other preprocessing.
type BouncerInterceptor struct {
	name     string
	priority int
	gw       *gateway.NenyaGateway
	logger   *slog.Logger
}

// NewBouncerInterceptor creates a new BouncerInterceptor.
func NewBouncerInterceptor(gw *gateway.NenyaGateway, logger *slog.Logger) *BouncerInterceptor {
	return &BouncerInterceptor{
		name:     "bouncer",
		priority: 50,
		gw:       gw,
		logger:   logger,
	}
}

func (b *BouncerInterceptor) Name() string  { return b.name }
func (b *BouncerInterceptor) Priority() int { return b.priority }
func (b *BouncerInterceptor) CanHandle(ctx context.Context, req *pipeline.InterceptRequest) bool {
	if ctx.Err() != nil {
		return false
	}
	if b.gw.Config.Bouncer.Enabled != nil && !*b.gw.Config.Bouncer.Enabled {
		return false
	}
	return req.SoftLimit > 0 && req.TokenCount >= req.SoftLimit
}

func (b *BouncerInterceptor) Process(ctx context.Context, req *pipeline.InterceptRequest) (*pipeline.InterceptResult, error) {
	if len(req.Messages) == 0 {
		return &pipeline.InterceptResult{Payload: req.Payload, Skip: true}, nil
	}

	lastMsg := req.Messages[len(req.Messages)-1]
	text, ok := lastMsg["content"].(string)
	if !ok || text == "" {
		return &pipeline.InterceptResult{Payload: req.Payload, Skip: true}, nil
	}

	var actualHardLimit int
	if b.gw.Config.Context.HardLimitTokens > 0 {
		actualHardLimit = b.gw.Config.Context.HardLimitTokens
	} else {
		actualHardLimit = req.HardLimit
	}

	lastMsg, text, skip := b.trimBeforeEngine(req, lastMsg, text, actualHardLimit)
	if skip {
		return &pipeline.InterceptResult{Payload: req.Payload, Skip: true}, nil
	}

	b.gw.Metrics.RecordInterception("engine")
	summarized, err := b.summarize(ctx, text, req.Profile.IsIDE)
	if err != nil || strings.TrimSpace(summarized) == "" {
		// Empty engine output would destroy the message content for just
		// the header — treat it as a summarize failure. The payload keeps
		// any trim that already happened (Truncated reports it).
		b.logger.Warn("engine summarization failed", "err", err, "reason", "empty_engine_output")
		return &pipeline.InterceptResult{
			Payload:   req.Payload,
			Truncated: true,
			Reason:    "trim",
			Skip:      true,
		}, nil
	}

	sanitized := "[Nenya Sanitized via Ollama]:\n" + summarized
	lastMsg["content"] = sanitized
	// In-place mutation; payload["messages"] keeps its original
	// []interface{} type for downstream consumers.
	b.gw.Metrics.RecordTokensSaved("bouncer", b.gw.CountTokens(text)-b.gw.CountTokens(sanitized))

	return &pipeline.InterceptResult{
		Payload:   req.Payload,
		Truncated: true,
		Reason:    "engine",
	}, nil
}

// trimBeforeEngine trims the payload to the hard limit when the last
// message exceeds it, re-reading the authoritative last message afterwards
// (TrimPayload may replace the message map inside the payload slice, while
// req.Messages holds stale pointers to the pre-trim maps). Returns the
// possibly-updated last message, its text, and skip=true when the trimmed
// payload's last message has no string content to summarize.
func (b *BouncerInterceptor) trimBeforeEngine(req *pipeline.InterceptRequest, lastMsg map[string]any, text string, actualHardLimit int) (map[string]any, string, bool) {
	contentTokens := b.gw.CountTokens(text)
	if contentTokens <= actualHardLimit {
		return lastMsg, text, false
	}
	modified, saved := pipeline.TrimPayload(b.logger, req.Payload, actualHardLimit, b.gw.CountTokens, b.gw.Config.Context)
	if !modified {
		return lastMsg, text, false
	}
	b.gw.Metrics.RecordTokensSaved("trim", saved)
	m, text, ok := resolvePostTrimMessage(req.Payload)
	if !ok {
		return nil, "", true
	}
	return m, text, false
}

// resolvePostTrimMessage reads the authoritative last message from the
// payload after TrimPayload may have replaced the messages slice. It fails
// (ok=false) when the new last message has no string content to summarize
// — e.g. assistant "content": null after an oversized tool exchange was
// dropped whole.
func resolvePostTrimMessage(payload map[string]any) (map[string]any, string, bool) {
	msgs, ok := payload["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		return nil, "", false
	}
	m, ok := msgs[len(msgs)-1].(map[string]any)
	if !ok {
		return nil, "", false
	}
	// Guard: a trailing trim unit can leave a system-role message last
	// (TrimPayload drops all non-system messages when the newest unit
	// alone exceeds the budget). Overwriting the system prompt with an
	// engine summary would corrupt the payload.
	if role, _ := m["role"].(string); role == "system" {
		return nil, "", false
	}
	rawText, ok := m["content"].(string)
	if !ok || rawText == "" {
		return nil, "", false
	}
	return m, rawText, true
}

func (b *BouncerInterceptor) summarize(ctx context.Context, heavyText string, isIDE bool) (string, error) {
	if len(b.gw.Config.Bouncer.Engine.ResolvedTargets) == 0 {
		return "", errors.New("bouncer engine: no resolved targets")
	}

	defaultPrompt := "You are a data privacy filter. Review the following text and remove or replace any IP addresses, AWS keys (AKIA...), passwords, tokens, or credentials with [REDACTED]. Preserve the original structure, detail level, and all non-sensitive content exactly as provided. Do NOT summarize or shorten the text."

	if isIDE && pipeline.HasCodeFences(heavyText) {
		defaultPrompt = "You are a data privacy filter for code. The following text contains code blocks (marked with ``` fences). Remove or replace any IP addresses, AWS keys (AKIA...), passwords, tokens, or credentials that appear OUTSIDE code blocks with [REDACTED]. Inside code blocks, only redact actual hardcoded secrets in string literals — preserve all code structure, function signatures, import statements, variable names, and line-number references exactly. Do NOT summarize, shorten, or restructure any code. Do NOT modify non-sensitive code."
	}

	ref := b.gw.Config.Bouncer.Engine
	systemPrompt, err := config.LoadPromptFile(ref.SystemPromptFile, ref.SystemPrompt, defaultPrompt)
	if err != nil {
		b.logger.Warn("failed to load privacy filter prompt, using default", "err", err)
		systemPrompt = defaultPrompt
	}

	agentName := ref.AgentName
	if agentName == "" {
		agentName = "inline"
	}

	return pipeline.CallEngineChain(ctx, b.gw.ClientFor,
		ref.ResolvedTargets, b.logger,
		func(providerName string, headers http.Header) error {
			return routing.InjectAPIKeyWithGateway(providerName, b.gw, headers)
		},
		"bouncer", agentName, systemPrompt, heavyText)
}
