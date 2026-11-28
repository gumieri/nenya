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
	audit    RedactionAuditSink
}

// Redaction surfaces the audit sink distinguishes.
const (
	RedactionSurfaceBody = "body"
	RedactionSurfaceArgs = "args"
)

// RedactionAuditSink receives structure-only redaction events for the
// tamper-evident audit log (NENYA-362): the agent identity, the surface
// constant, and the replacement count. No parameter carries the
// redacted content, so an implementation cannot leak what it never
// receives. Optional — a RedactInterceptor without a sink behaves
// exactly as before.
type RedactionAuditSink interface {
	RecordRedactionEvent(agent string, surface string, count int)
}

// SetRedactionAudit arms the audit sink (nil disarms). Called at
// construction time by the gateway wiring; not safe for concurrent use
// with in-flight Process calls, like the rest of the chain assembly.
func (r *RedactInterceptor) SetRedactionAudit(sink RedactionAuditSink) {
	r.audit = sink
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

// Strict implements StrictInterceptor: redaction is a security surface —
// an operational failure must not forward unredacted content.
func (r *RedactInterceptor) Strict() bool { return true }

// deterministicSecurity marks redaction for the MCP loop's rescan subset.
func (r *RedactInterceptor) deterministicSecurity() {}

// emitAuditEvents hands the armed sink one structure-only event per
// non-zero surface; a nil sink (audit disabled) is a no-op.
func (r *RedactInterceptor) emitAuditEvents(agent string, bodyCount, argsCount int) {
	if r.audit == nil {
		return
	}
	if bodyCount > 0 {
		r.audit.RecordRedactionEvent(agent, RedactionSurfaceBody, bodyCount)
	}
	if argsCount > 0 {
		r.audit.RecordRedactionEvent(agent, RedactionSurfaceArgs, argsCount)
	}
}

func (r *RedactInterceptor) CanHandle(_ context.Context, req *InterceptRequest) bool {
	return r.enabled && len(r.patterns) > 0 && len(req.Messages) > 0
}

func (r *RedactInterceptor) Process(_ context.Context, req *InterceptRequest) (*InterceptResult, error) {
	modified := false
	bodyRedactions, argsRedactions := 0, 0
	redact := newCountingRedactor(func(content string) string {
		// Enabled is already gated by CanHandle; passing true keeps the
		// count closure honest if the field semantics ever change.
		return RedactSecrets(content, true, r.patterns, r.label)
	}, r.label, &bodyRedactions)
	// NENYA-348: the tool-call arguments surface is a JSON-ENCODED
	// document — it gets the escape-aware redactor so a secret hidden
	// behind \" or \uXXXX quoting is still caught (the decoded span is
	// replaced, leaving the document's structure untouched).
	argsRedact := newCountingRedactor(func(content string) string {
		return RedactJSONEncoded(content, true, r.patterns, r.label)
	}, r.label, &argsRedactions)
	for _, msg := range req.Messages {
		if WalkMessageTextArgs(msg, redact, argsRedact) {
			modified = true
		}
	}
	redactions := bodyRedactions + argsRedactions
	if redactions > 0 {
		r.metrics.RecordRedaction(redactions)
		// NENYA-353: raise the sensitivity signal in place — the
		// tool-egress policy consumes it as the strong half of its
		// sensitive-request trigger.
		req.SecretsRedacted = true
		// NENYA-362: structure-only audit events (agent, surface, count —
		// never the redacted content) when the tamper-evident audit log
		// is armed. Nil-safe: unset sink means no audit surface.
		r.emitAuditEvents(req.AgentName, bodyRedactions, argsRedactions)
	}
	if !modified {
		return &InterceptResult{Payload: req.Payload, Skip: true}, nil
	}
	// Mutations are applied in place; payload["messages"] keeps its
	// original []interface{} type for downstream consumers.
	return &InterceptResult{
		Payload:   req.Payload,
		Truncated: true,
		Reason:    "redacted",
	}, nil
}
