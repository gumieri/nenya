package stream

import "testing"

func TestExtractThinkingSignal(t *testing.T) {
	tests := []struct {
		name           string
		chunk          map[string]interface{}
		wantActive     bool
		wantHasSignal  bool
	}{
		{
			name:           "empty delta reasoning content",
			chunk:          map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"reasoning_content": ""}}}},
			wantActive:     false,
			wantHasSignal:  true,
		},
		{
			name:           "non-empty delta reasoning content",
			chunk:          map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"reasoning_content": "thinking"}}}},
			wantActive:     true,
			wantHasSignal:  true,
		},
		{
			name:           "anthropic content_block_start thinking",
			chunk:          map[string]interface{}{"type": "content_block_start", "index": 0, "content_block": map[string]interface{}{"type": "thinking", "text": "foo"}},
			wantActive:     true,
			wantHasSignal:  true,
		},
		{
			name:           "anthropic content_block_delta thinking_delta",
			chunk:          map[string]interface{}{"type": "content_block_delta", "index": 0, "delta": map[string]interface{}{"type": "thinking_delta", "text": "bar"}},
			wantActive:     true,
			wantHasSignal:  true,
		},
		{
			name:           "anthropic content_block_start text",
			chunk:          map[string]interface{}{"type": "content_block_start", "content_block": map[string]interface{}{"type": "text", "text": "hello"}},
			wantActive:     false,
			wantHasSignal:  true,
		},
		{
			name:           "anthropic content_block_start tool_use",
			chunk:          map[string]interface{}{"type": "content_block_start", "content_block": map[string]interface{}{"type": "tool_use", "id": "123"}},
			wantActive:     false,
			wantHasSignal:  true,
		},
		{
			name:           "fallback chunk",
			chunk:          map[string]interface{}{"foo": "bar"},
			wantActive:     false,
			wantHasSignal:  false,
		},
		{
			name:           "nil chunk",
			chunk:          nil,
			wantActive:     false,
			wantHasSignal:  false,
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
