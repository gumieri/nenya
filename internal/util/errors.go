package util

import (
	"context"
	"errors"
	"strings"
)

// IsContextLengthError detects context-length exceeded errors from upstream providers.
// It checks the status code (400, 413, 422) and parses the response body for known
// error patterns indicating the prompt exceeds the model's context window.
func IsContextLengthError(status int, body string) bool {
	if status != 400 && status != 413 && status != 422 {
		return false
	}
	lower := strings.ToLower(body)
	patterns := []string{
		"context length is only ",
		"context window exceeds limit",
		"context length exceeded",
		"context_length_exceeded",
		"exceeded model token limit",
		"exceeds the available context size",
		"input length exceeds context length",
		"max_context_length",
		"maximum context length is ",
		"maximum prompt length is ",
		"model_context_window_exceeded",
		"prompt exceeds maximum context length",
		"prompt too long",
		"reduce the length of the messages",
		"this model's maximum context length",
		"tokens in request more than max tokens allowed",
		"too large for model with ",
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
