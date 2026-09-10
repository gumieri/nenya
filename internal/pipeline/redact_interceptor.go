package pipeline

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/nenya/internal/infra"
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
	metrics  *infra.Metrics
}

// NewRedactInterceptor creates a new RedactInterceptor. The metrics
// receiver is nil-safe; redaction counts are recorded through it.
func NewRedactInterceptor(enabled bool, patterns []*regexp.Regexp, label string, logger *slog.Logger, metrics *infra.Metrics) *RedactInterceptor {
	return &RedactInterceptor{
		name:     "redact",
		priority: 10,
		enabled:  enabled,
		patterns: patterns,
		label:    label,
		logger:   logger,
		metrics:  metrics,
	}
}

func (r *RedactInterceptor) Name() string  { return r.name }
func (r *RedactInterceptor) Priority() int { return r.priority }
func (r *RedactInterceptor) CanHandle(_ context.Context, req *InterceptRequest) bool {
	return r.enabled && len(r.patterns) > 0 && len(req.Messages) > 0
}

func (r *RedactInterceptor) Process(_ context.Context, req *InterceptRequest) (*InterceptResult, error) {
	modified := false
	redactions := 0
	for _, msg := range req.Messages {
		content, ok := msg["content"].(string)
		if !ok {
			continue
		}
		redacted := RedactSecrets(content, r.enabled, r.patterns, r.label)
		if redacted != content {
			// Count actual substitutions, not pattern hits: patterns may
			// overlap and double-count a single redacted span.
			redactions += strings.Count(redacted, r.label) - strings.Count(content, r.label)
			msg["content"] = redacted
			modified = true
		}
	}
	if r.metrics != nil && redactions > 0 {
		r.metrics.RecordRedaction(redactions)
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
