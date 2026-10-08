package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/tracing"
)

// Interceptor defines a preprocessing step that can inspect and modify
// the request payload before forwarding to upstream providers.
type Interceptor interface {
	// Name returns the interceptor identifier for logging/metrics.
	Name() string

	// CanHandle returns true if this interceptor should run for the given request.
	// Implementations should check for cancellation (ctx.Err()) to respect timeouts.
	CanHandle(ctx context.Context, req *InterceptRequest) bool

	// Process performs the interception. Returns the processed payload or an error.
	// On error, the chain falls back to the next interceptor unless the
	// interceptor (or the chain) is strict.
	// The caller must pass the request context to respect deadlines.
	Process(ctx context.Context, req *InterceptRequest) (*InterceptResult, error)

	// Priority determines ordering (lower numbers run first).
	Priority() int
}

// StrictInterceptor is an optional Interceptor extension: interceptors
// implementing it declare that an operational Process error must abort
// the request instead of falling through to the next interceptor.
// Security interceptors (redaction, spotlighting, injection detection)
// implement it with Strict() == true so a broken security layer cannot
// silently pass traffic; token-saving interceptors omit it (fail-open).
// Enforcement decisions use RejectError, which aborts regardless of
// strictness — Strict() covers only unexpected operational failures.
type StrictInterceptor interface {
	Interceptor
	// Strict reports whether an operational Process error must abort
	// the request with a structured error.
	Strict() bool
}

// InterceptRequest represents a request being processed by the interceptor chain.
type InterceptRequest struct {
	// Payload is the full request payload map (includes "messages", "model", etc.)
	Payload map[string]any

	// Messages is the parsed messages array from payload["messages"]
	Messages []map[string]any

	// AgentName is the canonical agent identity for this request (the
	// top-level model field as resolved by the proxy). Empty for
	// direct-built requests; interceptors fall back to payload["model"].
	AgentName string

	// Agent is the resolved agent config when AgentName names a
	// configured agent; nil otherwise. Consumers must nil-check.
	Agent *config.AgentConfig

	// Profile describes the client profile (IDE vs non-IDE)
	Profile ClientProfile

	// SoftLimit is the token threshold for triggering engine summarization
	SoftLimit int

	// HardLimit is the absolute maximum token count before truncation
	HardLimit int

	// TokenCount is the current total token count of messages
	TokenCount int

	// EntropyRedacted is set in place by the entropy interceptor when
	// it redacted high-entropy spans from this request; consumed by the
	// egress screen as a deterministic trigger signal.
	EntropyRedacted bool

	// SkipTFIDFPrune is set by the proxy when the per-agent cache_aware policy
	// is active for a cache-rich provider and the payload fits the hard limit:
	// the TF-IDF tail prune is skipped so the provider prefix cache can do the
	// work. The history-wide window compaction is bypassed separately in the
	// proxy (applyWindowStage). Security interceptors always run.
	SkipTFIDFPrune bool

	// DisableEscalation suppresses the injection interceptor's tier-2 LLM
	// escalation for this request: the ambiguous detection band falls back
	// to the deterministic act verdict instead of opening a classifier hop,
	// while the below-min pass band still passes. Set by surfaces that
	// rescan appended content mid-conversation (the MCP loop's tool-result
	// re-scan): the escalator is a request-time budget surface and must not
	// open LLM classifier hops inside the tool loop.
	DisableEscalation bool
}

// AgentNameFor returns the canonical agent name for an intercepted
// request: the proxy-resolved AgentName when set, otherwise the
// payload's top-level model field (the wire-format agent identity).
func AgentNameFor(req *InterceptRequest) string {
	if req.AgentName != "" {
		return req.AgentName
	}
	name, _ := req.Payload["model"].(string)
	return name
}

// InterceptResult represents the outcome of an interceptor's processing.
type InterceptResult struct {
	// Payload is the modified payload (may be identical to request if no changes)
	Payload map[string]any

	// Truncated indicates if the interceptor reduced content size
	Truncated bool

	// TokenCount is the new token count after processing (0 if unchanged)
	TokenCount int

	// Reason describes what the interceptor did (for logging/metrics)
	Reason string

	// Skip indicates if the interceptor explicitly skipped this request
	Skip bool
}

// InterceptorChain manages a prioritized list of interceptors.
// All methods are safe for concurrent use once built (immutable after build).
type InterceptorChain struct {
	interceptors []Interceptor
	strict       bool
	logger       *slog.Logger
	metrics      *infra.Metrics
	// stageName labels the chain in OTel-lite traces (NENYA-140); empty
	// uses the default "pipeline.interceptors". Sub-chains with distinct
	// semantics (e.g. the MCP loop's security re-scan) set their own.
	stageName string
}

// NewInterceptorChain creates a new interceptor chain.
func NewInterceptorChain(logger *slog.Logger) *InterceptorChain {
	return NewInterceptorChainWithMetrics(logger, nil)
}

// NewInterceptorChainWithMetrics creates a new interceptor chain with metrics support.
func NewInterceptorChainWithMetrics(logger *slog.Logger, metrics *infra.Metrics) *InterceptorChain {
	return &InterceptorChain{
		logger:  logger,
		metrics: metrics,
	}
}

// Register adds an interceptor and re-sorts by priority (stable: equal
// priorities keep registration order).
func (c *InterceptorChain) Register(interceptor Interceptor) {
	c.interceptors = append(c.interceptors, interceptor)
	sort.SliceStable(c.interceptors, func(i, j int) bool {
		return c.interceptors[i].Priority() < c.interceptors[j].Priority()
	})
}

// SetStrictMode when true causes ANY interceptor error to block the
// request, overriding per-interceptor strictness. Build-time knob: call
// during chain assembly, before the chain starts serving (the chain is
// otherwise immutable after build).
func (c *InterceptorChain) SetStrictMode(strict bool) {
	c.strict = strict
}

// StrictAbortMessage is the client-facing text used for strict chain
// aborts; writePipelineRejection falls back to it when a StrictError
// carries no message.
const StrictAbortMessage = "request aborted: preprocessing failed"

// RejectError wraps a pipeline policy rejection that must abort the request
// with a structured client error regardless of chain strict mode. Security
// interceptors return it for enforcement decisions (e.g. strict injection
// mode); operational failures use plain errors and follow strict-mode
// fallback semantics. Kind and Message carry the structured error surface.
type RejectError struct {
	Err     error
	Kind    infra.ErrorKind
	Message string
}

func (e *RejectError) Error() string { return e.Err.Error() }

func (e *RejectError) Unwrap() error { return e.Err }

// StrictError signals an operational interceptor failure under strict
// (fail-closed) semantics: the chain aborted and the caller must render
// a structured client error instead of proceeding with a payload that a
// security interceptor could not process. Kind defaults to
// ErrorKindInternal when zero; Message carries the client-facing text.
type StrictError struct {
	Err         error
	Kind        infra.ErrorKind
	Interceptor string
	Message     string
}

func (e *StrictError) Error() string { return e.Err.Error() }

func (e *StrictError) Unwrap() error { return e.Err }

// Execute runs all interceptors in priority order. Each successful interceptor
// mutates the request's Payload map in-place. On failure, behavior depends on
// strictness: an interceptor implementing StrictInterceptor with Strict() true
// (or the chain-level StrictMode) aborts the request with a *StrictError;
// non-strict interceptors fall through to the next one. A *RejectError always
// aborts the request — policy rejections are decisions, not operational
// failures. Execute always checks ctx cancellation at each interceptor
// boundary. The returned InterceptResult is a final-state snapshot only:
// per-interceptor Truncated/Reason/TokenCount are not aggregated — consumers
// must rely on req.Payload mutation and req.TokenCount.
func (c *InterceptorChain) Execute(ctx context.Context, req *InterceptRequest) (*InterceptResult, error) {
	// NENYA-140: the interceptor chain is a traced stage — one span for
	// the whole chain (per-interceptor detail stays in the existing debug
	// logs), recorded into the request summary when a trace is active.
	stage := c.stageName
	if stage == "" {
		stage = "pipeline.interceptors"
	}
	ctx, span := tracing.StartSpan(ctx, c.logger, stage)
	defer span.End()
	tracing.RecordStage(ctx, stage)

	for _, interceptor := range c.interceptors {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		if !interceptor.CanHandle(ctx, req) {
			c.logger.DebugContext(ctx, "interceptor skipped", "name", interceptor.Name())
			continue
		}

		start := time.Now()
		result, err := interceptor.Process(ctx, req)
		duration := time.Since(start)

		if c.metrics != nil {
			c.metrics.RecordInterceptorDuration(interceptor.Name(), duration)
		}

		if err != nil {
			var reject *RejectError
			if errors.As(err, &reject) {
				// Policy rejections are enforcement decisions, not
				// operational failures: abort without the error metric.
				return nil, err
			}
			if c.metrics != nil {
				c.metrics.RecordInterceptorError(interceptor.Name())
			}
			c.logger.WarnContext(ctx, "interceptor failed", "name", interceptor.Name(), "err", err, "duration_ms", duration.Milliseconds())
			if c.strict || interceptorStrict(interceptor) {
				return nil, &StrictError{
					Err:         fmt.Errorf("interceptor %q failed: %w", interceptor.Name(), err),
					Kind:        infra.ErrorKindInternal,
					Interceptor: interceptor.Name(),
					Message:     StrictAbortMessage,
				}
			}
			continue
		}

		if result.Skip {
			c.logger.DebugContext(ctx, "interceptor skipped processing", "name", interceptor.Name())
			continue
		}

		if result.TokenCount > 0 {
			// Keep req.TokenCount current so downstream CanHandle checks
			// (e.g. bouncer soft-limit gating) act on post-prune sizes.
			req.TokenCount = result.TokenCount
		}

		if c.metrics != nil {
			c.metrics.RecordInterceptorApplied(interceptor.Name())
		}

		c.logger.DebugContext(ctx, "interceptor applied",
			"name", interceptor.Name(),
			"truncated", result.Truncated,
			"tokens", result.TokenCount,
			"reason", result.Reason,
			"duration_ms", duration.Milliseconds())

		if result.Payload != nil {
			req.Payload = result.Payload
			// NOTE: req.Messages is constructed once by the caller.
			// Interceptors mutate message maps in place; the single
			// exception is BouncerInterceptor, whose TrimPayload call may
			// replace payload["messages"] with a new slice — bouncer is
			// therefore the last-registered interceptor and must re-read
			// the authoritative message from req.Payload afterwards.
		}
	}

	return &InterceptResult{
		Payload:    req.Payload,
		Truncated:  false,
		TokenCount: req.TokenCount,
		Reason:     "passthrough",
	}, nil
}

// List returns all registered interceptors.
func (c *InterceptorChain) List() []Interceptor {
	return c.interceptors
}

// deterministicSecurityInterceptor marks the fail-closed deterministic
// security interceptors that make up the MCP loop's rescan subset
// (redaction, entropy, tier-1 injection). Membership is opt-in by
// construction: a new security stage only joins the rescan by implementing
// the marker — review every new strict security interceptor for marker
// membership (spotlighting is a deliberate non-member: the MCP path
// spotlights results itself before the rescan runs).
type deterministicSecurityInterceptor interface {
	Interceptor
	deterministicSecurity()
}

// DeterministicSecuritySubset returns a new chain containing only the
// fail-closed deterministic security interceptors — redaction, entropy
// redaction, and tier-1 injection detection — preserving their relative
// order, logger, metrics, and strict mode. It exists for surfaces that
// append content to an already-intercepted conversation without re-running
// the full chain (the MCP multi-turn loop's tool-result re-scan): token
// economy stages (TF-IDF, bouncer) must not re-run, but appended content
// still gets the same deterministic scrubbing as client-supplied history.
// The injection interceptor runs in deterministic-only mode there: callers
// set InterceptRequest.DisableEscalation so no LLM classifier hop opens
// mid-loop. The returned chain shares interceptor instances with the parent
// — including its metrics, so rescan invocations count under the same
// nenya_interceptor_* series as request-path invocations (accepted: labels
// stay per-interceptor, not per-source). Safe for concurrent Execute calls
// exactly like the parent; nil-safe (returns an empty chain).
func (c *InterceptorChain) DeterministicSecuritySubset() *InterceptorChain {
	if c == nil {
		return NewInterceptorChainWithMetrics(nil, nil)
	}
	sub := NewInterceptorChainWithMetrics(c.logger, c.metrics)
	sub.strict = c.strict
	sub.stageName = "pipeline.mcp_rescan"
	for _, interceptor := range c.interceptors {
		if _, ok := interceptor.(deterministicSecurityInterceptor); ok {
			sub.interceptors = append(sub.interceptors, interceptor)
		}
	}
	return sub
}

// interceptorStrict reports whether the interceptor opted into
// fail-closed semantics via the optional StrictInterceptor interface.
func interceptorStrict(interceptor Interceptor) bool {
	strict, ok := interceptor.(StrictInterceptor)
	return ok && strict.Strict()
}
