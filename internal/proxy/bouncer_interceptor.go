package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"git.0ur.uk/nenya/config"
	"git.0ur.uk/nenya/internal/gateway"
	"git.0ur.uk/nenya/internal/pipeline"
	"git.0ur.uk/nenya/internal/routing"
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
	return req.TokenCount >= req.SoftLimit
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

	contentTokens := b.gw.CountTokens(text)
	if contentTokens > actualHardLimit {
		b.logger.Warn("payload exceeds hard limit, trimming before engine",
			"tokens", contentTokens, "hard_limit", actualHardLimit)
		modified, _ := pipeline.TrimPayload(b.logger, req.Payload, actualHardLimit, b.gw.CountTokens, b.gw.Config.Context)
		if modified {
			lastMsg = req.Messages[len(req.Messages)-1]
			if rawText, ok2 := lastMsg["content"].(string); ok2 {
				text = rawText
			}
		}
	}

	b.gw.Metrics.RecordInterception("engine")
	summarized, err := b.summarize(ctx, text, req.Profile.IsIDE)
	if err != nil {
		b.logger.Warn("engine summarization failed, proceeding with original", "err", err)
		return &pipeline.InterceptResult{Payload: req.Payload, Skip: true}, nil
	}

	lastMsg["content"] = fmt.Sprintf("[Nenya Sanitized via Ollama]:\n%s", summarized)
	req.Payload["messages"] = req.Messages

	return &pipeline.InterceptResult{
		Payload:   req.Payload,
		Truncated: true,
		Reason:    "engine",
	}, nil
}

func (b *BouncerInterceptor) summarize(ctx context.Context, heavyText string, isIDE bool) (string, error) {
	if len(b.gw.Config.Bouncer.Engine.ResolvedTargets) == 0 {
		return "", fmt.Errorf("bouncer engine: no resolved targets")
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

	return pipeline.CallEngineChain(ctx, b.gw.Client, b.gw.OllamaClient,
		ref.ResolvedTargets, b.logger,
		func(providerName string, headers http.Header) error {
			return routing.InjectAPIKeyWithGateway(providerName, b.gw, headers)
		},
		"bouncer", agentName, systemPrompt, heavyText)
}
