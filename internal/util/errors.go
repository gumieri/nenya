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
		"context_length_exceeded",
		"max_context_length",
		"context_length",
		"prompt too long",
		"this model's maximum context length",
		"maximum context length",
		"too many tokens",
		"prompt exceeds context",
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
