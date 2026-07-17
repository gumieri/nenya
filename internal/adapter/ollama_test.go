package adapter

import (
	"encoding/json"
	"testing"
)

// mapsEqualJSON compares two maps using JSON marshaling to avoid
// uncomparable slice comparison issues.
func mapsEqualJSON(got, want map[string]any) bool {
	gotB, err := json.Marshal(got)
	if err != nil {
		return false
	}
	wantB, err := json.Marshal(want)
	if err != nil {
		return false
	}
	return string(gotB) == string(wantB)
}

func TestStripToolChoice(t *testing.T) {
	tests := []struct {
		name    string
		input   map[string]any
		want    map[string]any
		changed bool
	}{
		{
			name: "strips tool_choice auto",
			input: map[string]any{
				"model":       "qwen2.5-coder",
				"messages":    []any{},
				"tool_choice": "auto",
				"tools":       []any{},
			},
			want: map[string]any{
				"model":    "qwen2.5-coder",
				"messages": []any{},
				"tools":    []any{},
			},
			changed: true,
		},
		{
			name: "strips tool_choice object",
			input: map[string]any{
				"model": "qwen3:14b",
				"messages": []any{
					map[string]any{"role": "user", "content": "hi"},
				},
				"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "weather"}},
				"tools": []any{
					map[string]any{"type": "function", "function": map[string]any{"name": "weather"}},
				},
			},
			want: map[string]any{
				"model": "qwen3:14b",
				"messages": []any{
					map[string]any{"role": "user", "content": "hi"},
				},
				"tools": []any{
					map[string]any{"type": "function", "function": map[string]any{"name": "weather"}},
				},
			},
			changed: true,
		},
		{
			name: "preserves tools without tool_choice",
			input: map[string]any{
				"model": "qwen2.5-coder",
				"messages": []any{
					map[string]any{"role": "user", "content": "test"},
				},
				"tools": []any{
					map[string]any{"type": "function", "function": map[string]any{"name": "webfetch"}},
				},
			},
			want: map[string]any{
				"model": "qwen2.5-coder",
				"messages": []any{
					map[string]any{"role": "user", "content": "test"},
				},
				"tools": []any{
					map[string]any{"type": "function", "function": map[string]any{"name": "webfetch"}},
				},
			},
			changed: false,
		},
		{
			name: "preserves stream_options with tools",
			input: map[string]any{
				"model":          "qwen3:14b",
				"messages":       []any{},
				"tools":          []any{},
				"stream_options": map[string]any{"include_usage": true},
			},
			want: map[string]any{
				"model":          "qwen3:14b",
				"messages":       []any{},
				"tools":          []any{},
				"stream_options": map[string]any{"include_usage": true},
			},
			changed: false,
		},
		{
			name:    "no-op for empty payload",
			input:   map[string]any{},
			want:    map[string]any{},
			changed: false,
		},
		{
			name: "preserves content arrays",
			input: map[string]any{
				"model": "qwen2.5-coder",
				"messages": []any{
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "text", "text": "test"},
							map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,abc"}},
						},
					},
				},
				"tool_choice": "auto",
			},
			want: map[string]any{
				"model": "qwen2.5-coder",
				"messages": []any{
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "text", "text": "test"},
							map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,abc"}},
						},
					},
				},
			},
			changed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := stripToolChoice(tt.input)
			if changed != tt.changed {
				t.Errorf("stripToolChoice() changed = %v, want %v", changed, tt.changed)
			}
			if !mapsEqualJSON(got, tt.want) {
				gotJSON, _ := json.MarshalIndent(got, "", "  ")
				wantJSON, _ := json.MarshalIndent(tt.want, "", "  ")
				t.Errorf("stripToolChoice() =\n%s\nwant\n%s", gotJSON, wantJSON)
			}
		})
	}
}

func TestOllamaAdapter_MutateRequest(t *testing.T) {
	adapter := NewOllamaAdapter()

	tests := []struct {
		name    string
		body    []byte
		model   string
		stream  bool
		wantErr bool
		check   func(t *testing.T, got []byte, original []byte)
	}{
		{
			name:    "strips tool_choice from valid JSON",
			body:    []byte(`{"model":"qwen2.5-coder","messages":[],"tool_choice":"auto","tools":[]}`),
			model:   "qwen2.5-coder",
			stream:  true,
			wantErr: false,
			check: func(t *testing.T, got []byte, original []byte) {
				var result map[string]any
				if err := json.Unmarshal(got, &result); err != nil {
					t.Fatalf("failed to unmarshal result: %v", err)
				}
				if _, exists := result["tool_choice"]; exists {
					t.Error("tool_choice field should be stripped")
				}
				if _, exists := result["tools"]; !exists {
					t.Error("tools field should be preserved")
				}
			},
		},
		{
			name:    "returns unchanged if tool_choice absent",
			body:    []byte(`{"model":"qwen3:14b","messages":[{"role":"user","content":"hi"}],"tools":[]}`),
			model:   "qwen3:14b",
			stream:  false,
			wantErr: false,
			check: func(t *testing.T, got []byte, original []byte) {
				var gotMap, origMap map[string]any
				json.Unmarshal(got, &gotMap)
				json.Unmarshal(original, &origMap)
				if !mapsEqualJSON(gotMap, origMap) {
					t.Errorf("body should be unchanged when tool_choice absent")
				}
			},
		},
		{
			name:    "handles invalid JSON gracefully",
			body:    []byte(`{invalid json`),
			model:   "qwen2.5-coder",
			stream:  true,
			wantErr: false,
			check: func(t *testing.T, got []byte, original []byte) {
				if string(got) != string(original) {
					t.Errorf("invalid JSON should be returned unchanged")
				}
			},
		},
		{
			name: "strips complex tool_choice object",
			body: []byte(`{
				"model": "qwen3:14b",
				"messages": [{"role": "user", "content": "test"}],
				"tool_choice": {"type": "function", "function": {"name": "weather"}},
				"tools": [{"type": "function", "function": {"name": "weather"}}]
			}`),
			model:   "qwen3:14b",
			stream:  true,
			wantErr: false,
			check: func(t *testing.T, got []byte, original []byte) {
				var result map[string]any
				if err := json.Unmarshal(got, &result); err != nil {
					t.Fatalf("failed to unmarshal result: %v", err)
				}
				if _, exists := result["tool_choice"]; exists {
					t.Error("complex tool_choice should be stripped")
				}
				if tools, exists := result["tools"]; !exists || tools == nil {
					t.Error("tools should be preserved")
				}
			},
		},
		{
			name:    "returns unchanged for nil body",
			body:    nil,
			model:   "qwen2.5-coder",
			stream:  true,
			wantErr: false,
			check: func(t *testing.T, got []byte, original []byte) {
				if got != nil {
					t.Error("nil body should remain nil")
				}
			},
		},
		{
			name:    "returns unchanged for non-object JSON",
			body:    []byte(`"not an object"`),
			model:   "qwen2.5-coder",
			stream:  false,
			wantErr: false,
			check: func(t *testing.T, got []byte, original []byte) {
				if string(got) != string(original) {
					t.Error("non-object JSON should be returned unchanged")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := adapter.MutateRequest(tt.body, tt.model, tt.stream)
			if (err != nil) != tt.wantErr {
				t.Errorf("OllamaAdapter.MutateRequest() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.check != nil {
				tt.check(t, got, tt.body)
			}
		})
	}
}
