package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/nenya/internal/infra"
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
	// On error, the chain falls back to the next interceptor unless StrictMode is enabled.
	// The caller must pass the request context to respect deadlines.
	Process(ctx context.Context, req *InterceptRequest) (*InterceptResult, error)

	// Priority determines ordering (lower numbers run first).
	Priority() int
}

// InterceptRequest represents a request being processed by the interceptor chain.
type InterceptRequest struct {
	// Payload is the full request payload map (includes "messages", "model", etc.)
	Payload map[string]any

	// Messages is the parsed messages array from payload["messages"]
	Messages []map[string]any

	// Profile describes the client profile (IDE vs non-IDE)
	Profile ClientProfile

	// SoftLimit is the token threshold for triggering engine summarization
	SoftLimit int

	// HardLimit is the absolute maximum token count before truncation
	HardLimit int

	// TokenCount is the current total token count of messages
	TokenCount int
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

// Register adds an interceptor and re-sorts by priority.
func (c *InterceptorChain) Register(interceptor Interceptor) {
	c.interceptors = append(c.interceptors, interceptor)
	sort.Slice(c.interceptors, func(i, j int) bool {
		return c.interceptors[i].Priority() < c.interceptors[j].Priority()
	})
}

// SetStrictMode when true causes interceptor errors to block the request.
// When false (default), errors fall back to the next interceptor.
func (c *InterceptorChain) SetStrictMode(strict bool) {
	c.strict = strict
}

// Execute runs all interceptors in priority order. Each successful interceptor
// mutates the request's Payload map in-place. On failure, behavior depends on
// strict mode: fallback to next interceptor (default) or return error (strict).
// Execute always checks ctx cancellation at each interceptor boundary.
func (c *InterceptorChain) Execute(ctx context.Context, req *InterceptRequest) (*InterceptResult, error) {
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
			if c.metrics != nil {
				c.metrics.RecordInterceptorError(interceptor.Name())
			}
			c.logger.WarnContext(ctx, "interceptor failed", "name", interceptor.Name(), "err", err, "duration_ms", duration.Milliseconds())
			if c.strict {
				return nil, fmt.Errorf("interceptor %q failed: %w", interceptor.Name(), err)
			}
			continue
		}

		if result.Skip {
			c.logger.DebugContext(ctx, "interceptor skipped processing", "name", interceptor.Name())
			continue
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
			if msgs, ok := result.Payload["messages"].([]map[string]any); ok {
				req.Messages = msgs
			}
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
