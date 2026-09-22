package pipeline

import (
	"context"
	"regexp"

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
	metrics  *infra.Metrics
}

// NewRedactInterceptor creates a new RedactInterceptor. The metrics
// receiver is nil-safe; redaction counts are recorded through it.
func NewRedactInterceptor(enabled bool, patterns []*regexp.Regexp, label string, metrics *infra.Metrics) *RedactInterceptor {
	return &RedactInterceptor{
		name:     "redact",
		priority: 10,
		enabled:  enabled,
		patterns: patterns,
		label:    label,
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
	redact := newCountingRedactor(func(content string) string {
		// Enabled is already gated by CanHandle; passing true keeps the
		// count closure honest if the field semantics ever change.
		return RedactSecrets(content, true, r.patterns, r.label)
	}, r.label, &redactions)
	for _, msg := range req.Messages {
		if WalkMessageText(msg, redact) {
			modified = true
		}
	}
	if redactions > 0 {
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
