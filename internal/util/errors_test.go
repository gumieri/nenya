package util

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestIsContextCanceled_Canceled(t *testing.T) {
	if !IsContextCanceled(context.Canceled) {
		t.Fatal("expected true for context.Canceled")
	}
}

func TestIsContextCanceled_DeadlineExceeded(t *testing.T) {
	if !IsContextCanceled(context.DeadlineExceeded) {
		t.Fatal("expected true for context.DeadlineExceeded")
	}
}

func TestIsContextCanceled_WrappedCanceled(t *testing.T) {
	wrapped := fmt.Errorf("upstream failed: %w", context.Canceled)
	if !IsContextCanceled(wrapped) {
		t.Fatal("expected true for wrapped context.Canceled")
	}
}

func TestIsContextCanceled_WrappedDeadlineExceeded(t *testing.T) {
	wrapped := fmt.Errorf("upstream timeout: %w", context.DeadlineExceeded)
	if !IsContextCanceled(wrapped) {
		t.Fatal("expected true for wrapped context.DeadlineExceeded")
	}
}

func TestIsContextCanceled_NilError(t *testing.T) {
	if IsContextCanceled(nil) {
		t.Fatal("expected false for nil error")
	}
}

func TestIsContextCanceled_GenericError(t *testing.T) {
	if IsContextCanceled(errors.New("connection refused")) {
		t.Fatal("expected false for generic error")
	}
}

func TestIsContextCanceled_MultiErrorCanceled(t *testing.T) {
	multi := errors.Join(context.Canceled, errors.New("some other error"))
	if !IsContextCanceled(multi) {
		t.Fatal("expected true for errors.Join containing context.Canceled")
	}
}

func TestIsContextCanceled_MultiErrorDeadlineExceeded(t *testing.T) {
	multi := errors.Join(context.DeadlineExceeded, errors.New("some other error"))
	if !IsContextCanceled(multi) {
		t.Fatal("expected true for errors.Join containing context.DeadlineExceeded")
	}
}

func TestIsContextCanceled_MultiErrorNoContext(t *testing.T) {
	multi := errors.Join(errors.New("err1"), errors.New("err2"))
	if IsContextCanceled(multi) {
		t.Fatal("expected false for errors.Join without context errors")
	}
}

// TestIsContextLengthError pins NENYA-15: overflow wordings from every
// provider family classify on 400/413/422, rate-limit/quota wording vetoes
// classification, and other statuses never match.
func TestIsContextLengthError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		// Real-world provider wordings.
		{"anthropic_prompt_is_too_long", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000 maximum"}}`, true},
		{"openai_maximum_context_length", 400, `{"error":{"message":"This model's maximum context length is 8192 tokens. However, you requested 10000 tokens."}}`, true},
		{"bedrock_input_is_too_long", 400, `{"message":"Input is too long for requested model."}`, true},
		{"gemini_exceeds_maximum_tokens", 400, `{"error":{"message":"The input token count (300000) exceeds the maximum number of tokens allowed (262144)."}}`, true},
		{"gemini_exceeds_maximum_tokens_422", 422, `{"error":{"message":"The input token count (300000) exceeds the maximum number of tokens allowed (262144)."}}`, true},
		{"glm_prompt_exceeds_length_limit", 400, `{"error":{"code":"1210","message":"your prompt exceeds the length limit"}}`, true},
		{"ollama_input_length_exceeds_context", 400, `{"error":"input length exceeds context length"}`, true},
		{"generic_too_many_tokens_413", 413, `{"error":"too many tokens"}`, true},
		{"snake_context_length_exceeded", 400, `{"error":{"code":"context_length_exceeded"}}`, true},
		{"payload_too_large_non_context_413", 413, `{"error":"payload too large"}`, false},
		// Status gate.
		{"context_wording_on_429", 429, `{"error":"prompt is too long"}`, false},
		{"context_wording_on_500", 500, `{"error":"prompt is too long"}`, false},
		// Rate-limit/quota veto: each case EMBEDS an overflow pattern so a
		// deleted veto would flip the result to true and fail.
		{"rate_limit_words_veto", 400, `{"error":{"message":"Rate limit reached: too many tokens per minute. Limit 30, try again in 20s."}}`, false},
		{"snake_rate_limit_veto", 400, `{"error":"rate_limit triggered: too many input tokens"}`, false},
		{"requests_per_minute_veto", 400, `{"error":"too many requests; exceeds the maximum number of tokens"}`, false},
		{"quota_veto", 400, `{"error":"quota exceeded: too many tokens"}`, false},
		// Non-overflow validation errors.
		{"normal_validation_error", 400, `{"error":{"message":"messages is required"}}`, false},
		{"empty_body", 400, ``, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsContextLengthError(tt.status, tt.body); got != tt.want {
				t.Errorf("IsContextLengthError(%d, %q) = %v; want %v", tt.status, tt.body, got, tt.want)
			}
		})
	}
}
