package stream

import (
	"context"
	"io"
	"strings"
	"testing"
)

// TestExtractReasoningTokens pins the NENYA-33 spelling matrix: every
// known provider spelling of the reasoning/thinking counter is extracted,
// zero values never register, and unknown shapes yield 0.
func TestExtractReasoningTokens(t *testing.T) {
	tests := []struct {
		name  string
		usage map[string]interface{}
		want  int
	}{
		{
			name:  "flat reasoning_tokens (DeepSeek/Zai style)",
			usage: map[string]interface{}{"reasoning_tokens": float64(42)},
			want:  42,
		},
		{
			name: "output_tokens_details.reasoning_tokens (OpenAI current)",
			usage: map[string]interface{}{
				"output_tokens_details": map[string]interface{}{"reasoning_tokens": float64(77)},
			},
			want: 77,
		},
		{
			name: "completion_tokens_details.reasoning_tokens (legacy OpenAI)",
			usage: map[string]interface{}{
				"completion_tokens_details": map[string]interface{}{"reasoning_tokens": float64(13)},
			},
			want: 13,
		},
		{
			name:  "completion_reasoning_tokens (Anthropic raw)",
			usage: map[string]interface{}{"completion_reasoning_tokens": float64(31)},
			want:  31,
		},
		{
			name:  "thoughtsTokenCount (Gemini native)",
			usage: map[string]interface{}{"thoughtsTokenCount": float64(55)},
			want:  55,
		},
		{
			name:  "all zero yields 0 (placeholder filter)",
			usage: map[string]interface{}{"reasoning_tokens": float64(0)},
			want:  0,
		},
		{
			name:  "empty usage",
			usage: map[string]interface{}{},
			want:  0,
		},
		{
			name: "flat wins when multiple spellings present",
			usage: map[string]interface{}{
				"reasoning_tokens":      float64(7),
				"output_tokens_details": map[string]interface{}{"reasoning_tokens": float64(9)},
			},
			want: 7,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractReasoningTokens(tt.usage); got != tt.want {
				t.Errorf("ExtractReasoningTokens() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestReaderOnUsageNestedReasoningDetails pins the streaming path: a usage
// chunk carrying only the nested OpenAI reasoning details still reaches
// OnUsage with the reasoning delta (previously silently dropped).
func TestReaderOnUsageNestedReasoningDetails(t *testing.T) {
	input := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":9,\"total_tokens\":14,\"output_tokens_details\":{\"reasoning_tokens\":6}}}\n\n" +
		"data: [DONE]\n\n"
	reader := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())

	var got UsageData
	called := false
	reader.SetOnUsage(func(u UsageData) {
		called = true
		got = u
	})
	if _, err := io.ReadAll(reader); err != nil && err != io.EOF {
		t.Fatalf("unexpected read error: %v", err)
	}
	if !called {
		t.Fatal("usage callback never fired")
	}
	if got.ReasoningTokens != 6 {
		t.Errorf("expected reasoning delta 6, got %d", got.ReasoningTokens)
	}
	if got.CompletionTokens != 9 {
		t.Errorf("expected completion delta 9, got %d", got.CompletionTokens)
	}
}
