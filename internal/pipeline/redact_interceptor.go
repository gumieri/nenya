package pipeline

import (
	"context"
	"log/slog"
	"regexp"
)

// RedactInterceptor performs pattern-based secret redaction on messages.
// Priority: 10 — runs first, before any other processing.
type RedactInterceptor struct {
	name     string
	priority int
	enabled  bool
	patterns []*regexp.Regexp
	label    string
	logger   *slog.Logger
}

// NewRedactInterceptor creates a new RedactInterceptor.
func NewRedactInterceptor(enabled bool, patterns []*regexp.Regexp, label string, logger *slog.Logger) *RedactInterceptor {
	return &RedactInterceptor{
		name:     "redact",
		priority: 10,
		enabled:  enabled,
		patterns: patterns,
		label:    label,
		logger:   logger,
	}
}

func (r *RedactInterceptor) Name() string  { return r.name }
func (r *RedactInterceptor) Priority() int { return r.priority }
func (r *RedactInterceptor) CanHandle(_ context.Context, req *InterceptRequest) bool {
	return r.enabled && len(r.patterns) > 0 && len(req.Messages) > 0
}

func (r *RedactInterceptor) Process(_ context.Context, req *InterceptRequest) (*InterceptResult, error) {
	modified := false
	for _, msg := range req.Messages {
		content, ok := msg["content"].(string)
		if !ok {
			continue
		}
		redacted := RedactSecrets(content, r.enabled, r.patterns, r.label)
		if redacted != content {
			msg["content"] = redacted
			modified = true
		}
	}
	if !modified {
		return &InterceptResult{Payload: req.Payload, Skip: true}, nil
	}
	req.Payload["messages"] = req.Messages
	return &InterceptResult{
		Payload:   req.Payload,
		Truncated: true,
		Reason:    "redacted",
	}, nil
}
