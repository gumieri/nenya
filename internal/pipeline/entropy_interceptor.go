package pipeline

import (
	"context"

	"github.com/nenya/internal/infra"
)

// EntropyInterceptor performs entropy-based high-entropy string redaction.
// Priority: 20 — runs after pattern redaction, before TF-IDF.
type EntropyInterceptor struct {
	name     string
	priority int
	filter   *EntropyFilter
	label    string
	metrics  *infra.Metrics
}

// NewEntropyInterceptor creates a new EntropyInterceptor. The metrics
// receiver is nil-safe; redaction counts are recorded through it.
func NewEntropyInterceptor(filter *EntropyFilter, label string, metrics *infra.Metrics) *EntropyInterceptor {
	return &EntropyInterceptor{
		name:     "entropy",
		priority: 20,
		filter:   filter,
		label:    label,
		metrics:  metrics,
	}
}

func (e *EntropyInterceptor) Name() string  { return e.name }
func (e *EntropyInterceptor) Priority() int { return e.priority }
func (e *EntropyInterceptor) CanHandle(_ context.Context, req *InterceptRequest) bool {
	return e.filter != nil && len(req.Messages) > 0
}

func (e *EntropyInterceptor) Process(_ context.Context, req *InterceptRequest) (*InterceptResult, error) {
	modified := false
	redactions := 0
	redact := newCountingRedactor(func(content string) string {
		return e.filter.RedactHighEntropy(content, e.label)
	}, e.label, &redactions)
	for _, msg := range req.Messages {
		if WalkMessageText(msg, redact) {
			modified = true
		}
	}
	if redactions > 0 {
		e.metrics.RecordRedaction(redactions)
	}
	if !modified {
		return &InterceptResult{Payload: req.Payload, Skip: true}, nil
	}
	// Mutations are applied in place; payload["messages"] keeps its
	// original []interface{} type for downstream consumers.
	return &InterceptResult{
		Payload:   req.Payload,
		Truncated: true,
		Reason:    "entropy_redacted",
	}, nil
}
