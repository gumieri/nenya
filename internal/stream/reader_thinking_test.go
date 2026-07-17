// Test file for SSETransformingReader thinking callback functionality.
// Tests the integration between thinking event detection and stall timer extension.
package stream

import (
	"context"
	"io"
	"testing"
)

type mockStallReader struct{}

func (r *mockStallReader) Read(p []byte) (n int, err error) {
	return 0, io.EOF
}

func (r *mockStallReader) Close() error {
	return nil
}

func TestSetOnThinking(t *testing.T) {
	ctx := context.Background()
	mockReader := &mockStallReader{}

	tr := &SSETransformingReader{
		ctx: ctx,
		src: mockReader,
	}

	var callbackActive bool
	var callbackCalled bool

	tr.SetOnThinking(func(active bool) {
		callbackActive = active
		callbackCalled = true
	})

	tests := []struct {
		name       string
		chunk      map[string]interface{}
		wantActive bool
		wantSignal bool
	}{
		{
			name:       "empty reasoning_content",
			chunk:      map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"reasoning_content": ""}}}},
			wantActive: false,
			wantSignal: true,
		},
		{
			name:       "non-empty reasoning_content",
			chunk:      map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"reasoning_content": "thinking"}}}},
			wantActive: true,
			wantSignal: true,
		},
		{
			name:       "anthropic thinking start",
			chunk:      map[string]interface{}{"type": "content_block_start", "content_block": map[string]interface{}{"type": "thinking", "text": "foo"}},
			wantActive: true,
			wantSignal: true,
		},
		{
			name:       "anthropic text start",
			chunk:      map[string]interface{}{"type": "content_block_start", "content_block": map[string]interface{}{"type": "text", "text": "hello"}},
			wantActive: false,
			wantSignal: true,
		},
		{
			name:       "no thinking signal",
			chunk:      map[string]interface{}{"type": "ping"},
			wantActive: false,
			wantSignal: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callbackCalled = false
			active, hasSignal := ExtractThinkingSignal(tt.chunk)
			if hasSignal {
				tr.callUsageAndContentCallbacks(tt.chunk)
			}
			if hasSignal && !callbackCalled {
				t.Errorf("SetOnThinking callback not called when signal detected")
			}
			if callbackCalled && callbackActive != tt.wantActive {
				t.Errorf("SetOnThinking callback active = %v, want %v", callbackActive, tt.wantActive)
			}
			if active != tt.wantActive {
				t.Errorf("ExtractThinkingSignal() active = %v, want %v", active, tt.wantActive)
			}
			if hasSignal != tt.wantSignal {
				t.Errorf("ExtractThinkingSignal() signal = %v, want %v", hasSignal, tt.wantSignal)
			}
		})
	}
}