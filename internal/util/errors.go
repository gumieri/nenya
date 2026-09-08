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
