package util

import (
	"context"
	"errors"
	"strings"
)

// rateLimitIndicators are error-body phrases that identify a rate-limit or
// quota failure. They veto context-overflow classification: several
// providers word TPM limits with phrases ("too many tokens") that would
// otherwise match an overflow pattern, and misrouting a rate limit into the
// summarization-retry path wastes an engine call.
var rateLimitIndicators = []string{
	"rate limit",
	"rate_limit",
	"too many requests",
	"quota",
	"tokens per minute",
	"requests per minute",
}

// modelNotFoundIndicators are error-body phrases that identify a
// model-not-found failure (NENYA-168): the requested model is not available on
// that provider, so the caller can fail over to a target that serves it. A bare
// 404 from an upstream on a model endpoint is treated as model-not-found by the
// callers; these phrases cover the 400/422 wordings.
var modelNotFoundIndicators = []string{
	"model_not_found",
	"not_found_error",
	"unknown_model",
	"unavailable_model",
	"model not found",
	"model does not exist",
	"model doesn't exist",
	"unknown model:",
	"no such model",
	"model is not available",
	"model not available",
	"model has been deprecated",
	"deprecated model",
}

// quotaWording vetoes model-not-found classification: a 4xx that says the
// account is out of quota is a billing failure, not a missing model.
var quotaWording = []string{"quota", "insufficient"}

// IsModelNotFoundStatus reports whether a status/body pair means "this model is
// not available on this provider" (NENYA-168): a 404 from an upstream model
// endpoint (a wrong path or an absent model), or a 400/422 whose body names a
// missing model. The circuit-breaker classifier and the proxy's failover
// decision share this predicate so the two taxonomies cannot drift.
func IsModelNotFoundStatus(status int, body string) bool {
	if status == 404 {
		return true
	}
	switch status {
	case 400, 422:
		return IsModelNotFoundMessage(body)
	}
	return false
}

// IsModelNotFoundMessage reports whether a bounded error body carries a
// model-not-found phrase (NENYA-168). Quota and context-length wording veto the
// classification: those are different failure classes with different handling,
// and a body can mention a model id while reporting either.
func IsModelNotFoundMessage(body string) bool {
	if body == "" {
		return false
	}
	lower := strings.ToLower(body)
	for _, p := range quotaWording {
		if strings.Contains(lower, p) {
			return false
		}
	}
	if IsContextLengthError(400, lower) {
		return false
	}
	for _, p := range modelNotFoundIndicators {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// overloadIndicators are the capacity/overload phrases an upstream uses when it
// is temporarily unable to serve a model (NENYA-168). They drive both the
// circuit breaker's capacity class and the stream-bootstrap rejection test, so
// one list keeps the two taxonomies aligned.
var overloadIndicators = []string{
	"at capacity",
	"capacity exceeded",
	"capacity is full",
	"overloaded",
	"overloaded_error",
	"overload_error",
}

// IsOverloadMessage reports whether a bounded error text carries a capacity or
// overload phrase (NENYA-168). The phrases are anchored ("at capacity",
// "overloaded") rather than bare substrings, so a model id like
// "capacity-3b" or a stray "capacity: 4096" field does not classify as an
// overload rejection.
func IsOverloadMessage(text string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	for _, p := range overloadIndicators {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// IsContextLengthError detects context-length exceeded errors from upstream providers.
// It checks the status code (400, 413, 422) and parses the response body for known
// error patterns indicating the prompt exceeds the model's context window.
// Rate-limit/quota wording vetoes the classification even when an overflow
// pattern also matches.
func IsContextLengthError(status int, body string) bool {
	if status != 400 && status != 413 && status != 422 {
		return false
	}
	lower := strings.ToLower(body)
	for _, p := range rateLimitIndicators {
		if strings.Contains(lower, p) {
			return false
		}
	}
	patterns := []string{
		"context length is only ",
		"context window exceeds limit",
		"context length exceeded",
		"context_length_exceeded",
		"exceeded model token limit",
		"exceeds the available context size",
		"prompt exceeds the length limit",
		"exceeds the maximum number of tokens",
		"input is too long",
		"input length exceeds context length",
		"max_context_length",
		"maximum context length is ",
		"maximum prompt length is ",
		"model_context_window_exceeded",
		"prompt exceeds maximum context length",
		"prompt is too long",
		"prompt too long",
		"reduce the length of the messages",
		"this model's maximum context length",
		"tokens in request more than max tokens allowed",
		"too large for model with ",
		"too many input tokens",
		"too many tokens",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// IsContextCanceled detects whether an error matches or wraps context.Canceled
// or context.DeadlineExceeded. Returns true for either context termination type,
// false for all other errors including nil.
func IsContextCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
