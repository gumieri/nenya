package proxy

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
)

// forwardOpts groups the shared parameters of the non-chat forward helpers
// (embeddings, System One): more than five positional parameters violated the
// options-struct rule (§11) once the TPM estimate joined (NENYA-336).
type forwardOpts struct {
	gw           *gateway.NenyaGateway
	respWriter   http.ResponseWriter
	requestCtx   context.Context
	endpointURL  string
	bodyBytes    []byte
	providerName string
	// baseURL keys the post-dispatch TPM true-up exactly like the
	// admission Check when the provider name is empty (NENYA-336 review).
	baseURL     string
	modelName   string
	srcHeaders  http.Header
	maxAttempts int
	keyRef      string
	// estimate is the admission-time token debit (NENYA-336); the forward
	// helpers true the TPM bucket up to the response's real input total.
	estimate int
}

// rateLimitRetryAfter is the client-facing retry hint on a non-chat 429.
const rateLimitRetryAfter = "60"

// writeRateLimitRejection renders a non-chat rate-limit rejection. The
// limiter's rejection details (dimension, limit, remaining budget) are
// logged server-side only — they describe the shared provider bucket, and
// exposing them would let any authenticated client probe co-tenant budget.
// The gateway-capacity dimension is not a rate limit: it renders as a 503
// without the invented retry hint. Rejection-metric conventions stay at the
// call sites (extensions counts by endpoint name, passthrough by host; the
// embeddings/systemone/files/responses sites record none — the shared
// bucket accounting lives in the rate-limit admission path itself, not
// per-route counters (nenya_ratelimit_rejected_total counts rejections
// only at sites that record it)).
func writeRateLimitRejection(logger *slog.Logger, w http.ResponseWriter, providerName string, rej infra.RateLimitRejection) {
	if rej.Dimension == infra.RateLimitDimensionBucketCapacity {
		// Gateway capacity, not a rate limit: 503 under ErrorKindInternal
		// with the rejection counted at sites that record it — the blur is
		// accepted project-wide (matches the chat path retry.go behavior).
		// Bucket internals stay out of the log line (the helper doc above);
		// the condition itself is rare and serious.
		logger.Warn("gateway rate-limit host capacity exhausted; rejection details withheld",
			"provider", providerName)
		writeStructuredError(w, http.StatusServiceUnavailable, infra.ErrorKindInternal, "Gateway capacity exhausted")
		return
	}
	logger.Warn("rate limit exceeded (non-chat endpoint)",
		"provider", providerName,
		"dimension", rej.Dimension, "limit", rej.Limit,
		"bucket_left", rej.BucketLeft, "requested_tokens", rej.TokenCount)
	w.Header().Set("Retry-After", rateLimitRetryAfter)
	writeStructuredError(w, http.StatusTooManyRequests, infra.ErrorKindRateLimited, "Rate limit exceeded")
}
