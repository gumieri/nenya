package stream

import (
	"regexp"
	"strings"
	"testing"
)

func TestExtractThinkingSignal(t *testing.T) {
	tests := []struct {
		name          string
		chunk         map[string]interface{}
		wantActive    bool
		wantHasSignal bool
	}{
		{
			name:          "empty delta reasoning content",
			chunk:         map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"reasoning_content": ""}}}},
			wantActive:    false,
			wantHasSignal: true,
		},
		{
			name:          "non-empty delta reasoning content",
			chunk:         map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"reasoning_content": "thinking"}}}},
			wantActive:    true,
			wantHasSignal: true,
		},
		{
			name:          "anthropic content_block_start thinking",
			chunk:         map[string]interface{}{"type": "content_block_start", "index": 0, "content_block": map[string]interface{}{"type": "thinking", "text": "foo"}},
			wantActive:    true,
			wantHasSignal: true,
		},
		{
			name:          "anthropic content_block_delta thinking_delta",
			chunk:         map[string]interface{}{"type": "content_block_delta", "index": 0, "delta": map[string]interface{}{"type": "thinking_delta", "text": "bar"}},
			wantActive:    true,
			wantHasSignal: true,
		},
		{
			name:          "anthropic content_block_start text",
			chunk:         map[string]interface{}{"type": "content_block_start", "content_block": map[string]interface{}{"type": "text", "text": "hello"}},
			wantActive:    false,
			wantHasSignal: true,
		},
		{
			name:          "anthropic content_block_start tool_use",
			chunk:         map[string]interface{}{"type": "content_block_start", "content_block": map[string]interface{}{"type": "tool_use", "id": "123"}},
			wantActive:    false,
			wantHasSignal: true,
		},
		{
			name:          "fallback chunk",
			chunk:         map[string]interface{}{"foo": "bar"},
			wantActive:    false,
			wantHasSignal: false,
		},
		{
			name:          "nil chunk",
			chunk:         nil,
			wantActive:    false,
			wantHasSignal: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotActive, gotHasSignal := ExtractThinkingSignal(tt.chunk)
			if gotActive != tt.wantActive {
				t.Errorf("ExtractThinkingSignal() active = %v, want %v", gotActive, tt.wantActive)
			}
			if gotHasSignal != tt.wantHasSignal {
				t.Errorf("ExtractThinkingSignal() hasSignal = %v, want %v", gotHasSignal, tt.wantHasSignal)
			}
		})
	}
}

func TestCheckContentBlockStart(t *testing.T) {
	tests := []struct {
		name          string
		chunk         map[string]interface{}
		wantActive    bool
		wantHasSignal bool
	}{
		{
			name:          "nil content_block",
			chunk:         map[string]interface{}{"type": "content_block_start"},
			wantActive:    false,
			wantHasSignal: false,
		},
		{
			name:          "content_block missing type",
			chunk:         map[string]interface{}{"type": "content_block_start", "content_block": map[string]interface{}{"text": "foo"}},
			wantActive:    false,
			wantHasSignal: false,
		},
		{
			name:          "type thinking",
			chunk:         map[string]interface{}{"type": "content_block_start", "content_block": map[string]interface{}{"type": "thinking", "text": "bar"}},
			wantActive:    true,
			wantHasSignal: true,
		},
		{
			name:          "type text",
			chunk:         map[string]interface{}{"type": "content_block_start", "content_block": map[string]interface{}{"type": "text", "text": "hello"}},
			wantActive:    false,
			wantHasSignal: true,
		},
		{
			name:          "type tool_use",
			chunk:         map[string]interface{}{"type": "content_block_start", "content_block": map[string]interface{}{"type": "tool_use", "id": "123"}},
			wantActive:    false,
			wantHasSignal: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotActive, gotHasSignal := checkContentBlockStart(tt.chunk)
			if gotActive != tt.wantActive {
				t.Errorf("checkContentBlockStart() active = %v, want %v", gotActive, tt.wantActive)
			}
			if gotHasSignal != tt.wantHasSignal {
				t.Errorf("checkContentBlockStart() hasSignal = %v, want %v", gotHasSignal, tt.wantHasSignal)
			}
		})
	}
}

// TestStreamFilterStraddlingSecret is the regression test for the
// boundary-straddling leak: a secret split across two SSE chunks must be
// fully redacted once the client concatenates the emitted chunks, and an
// in-chunk secret must not consume a straddling match's fragment.
func TestStreamFilterStraddlingSecret(t *testing.T) {
	secret := "AKIAIOSFODNN7EXAMPLE"
	patterns := []*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)}
	f := NewStreamFilter(patterns, nil, "[REDACTED]", 256)

	first := "intro " + secret[:17]
	out1, action1, _ := f.FilterContent(first)
	if action1 == ActionBlock {
		t.Fatalf("unexpected block on first chunk")
	}
	second := secret[17:] + " plus " + secret + " end"
	out2, action2, _ := f.FilterContent(second)

	combined := out1 + out2
	if strings.Contains(combined, secret) {
		t.Fatalf("straddling secret leaked across chunks: %q", combined)
	}
	if !strings.Contains(combined, "[REDACTED]") {
		t.Errorf("expected redaction marker in output, got %q", combined)
	}
	if action2 != ActionRedact && action1 != ActionRedact {
		t.Errorf("expected at least one ActionRedact, got %v then %v", action1, action2)
	}
}

// TestStreamFilterStraddlingIBANChecksum covers the fail-safe fallback: a
// genuine IBAN split across chunks must still be redacted even though the
// fused regex candidate cannot pass checksum validation.
func TestStreamFilterStraddlingIBANChecksum(t *testing.T) {
	iban := "DE89370400440532013000"
	patterns := []*regexp.Regexp{regexp.MustCompile(`(?i)\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,4})?\b`)}
	f := NewStreamFilter(patterns, nil, "[REDACTED]", 256)

	out1, _, _ := f.FilterContent("pay to " + iban[:12])
	out2, _, _ := f.FilterContent(iban[12:] + " today")
	if strings.Contains(out1+out2, iban) {
		t.Fatalf("straddling IBAN leaked: %q", out1+out2)
	}
}
