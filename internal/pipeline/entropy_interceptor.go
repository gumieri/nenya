package pipeline

import (
	"context"
	"log/slog"
)

// EntropyInterceptor performs entropy-based high-entropy string redaction.
// Priority: 20 — runs after pattern redaction, before TF-IDF.
type EntropyInterceptor struct {
	name     string
	priority int
	filter   *EntropyFilter
	label    string
	logger   *slog.Logger
}

// NewEntropyInterceptor creates a new EntropyInterceptor.
func NewEntropyInterceptor(filter *EntropyFilter, label string, logger *slog.Logger) *EntropyInterceptor {
	return &EntropyInterceptor{
		name:     "entropy",
		priority: 20,
		filter:   filter,
		label:    label,
		logger:   logger,
	}
}

func (e *EntropyInterceptor) Name() string  { return e.name }
func (e *EntropyInterceptor) Priority() int { return e.priority }
func (e *EntropyInterceptor) CanHandle(_ context.Context, req *InterceptRequest) bool {
	return e.filter != nil && len(req.Messages) > 0
}

func (e *EntropyInterceptor) Process(_ context.Context, req *InterceptRequest) (*InterceptResult, error) {
	modified := false
	for _, msg := range req.Messages {
		content, ok := msg["content"].(string)
		if !ok {
			continue
		}
		redacted := e.filter.RedactHighEntropy(content, e.label)
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
		Reason:    "entropy_redacted",
	}, nil
}
