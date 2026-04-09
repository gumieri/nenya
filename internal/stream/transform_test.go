package stream

import (
	"bytes"
	"encoding/json"
	"testing"
)

func assertEqualJSON(t *testing.T, got, want []byte) {
	t.Helper()

	var g, w interface{}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("failed to unmarshal got: %v", err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("failed to unmarshal want: %v", err)
	}

	gj, _ := json.Marshal(g)
	wj, _ := json.Marshal(w)

	if string(gj) != string(wj) {
		t.Errorf("JSON mismatch\ngot:  %s\nwant: %s", gj, wj)
	}
}

func assertEqualBytes(t *testing.T, got, want []byte) {
	t.Helper()
	if string(got) != string(want) {
		t.Errorf("bytes mismatch\ngot:  %q\nwant: %q", got, want)
	}
}

func TestGeminiTransformer_TransformSSEChunk(t *testing.T) {
	transformer := &GeminiTransformer{}

	tests := []struct {
		name string
		data []byte
		want []byte
	}{
		{
			name: "empty data passes through",
			data: []byte(""),
			want: []byte(""),
		},
		{
			name: "non-JSON passes through",
			data: []byte("[DONE]"),
			want: []byte("[DONE]"),
		},
		{
			name: "JSON without tool_calls unchanged",
			data: []byte(`{"choices":[{"delta":{"content":"hello"}}]}`),
			want: []byte(`{"choices":[{"delta":{"content":"hello"}}]}`),
		},
		{
			name: "Gemini tool_calls without index gets index added",
			data: []byte(`{"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"foo","arguments":""}}]}}]}`),
			want: []byte(`{"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"foo","arguments":""},"index":0}]}}]}`),
		},
		{
			name: "Gemini tool_calls with multiple items get sequential indices",
			data: []byte(`{"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"a","arguments":""}},{"id":"call_2","type":"function","function":{"name":"b","arguments":""}}]}}]}`),
			want: []byte(`{"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"a","arguments":""},"index":0},{"id":"call_2","type":"function","function":{"name":"b","arguments":""},"index":1}]}}]}`),
		},
		{
			name: "Gemini tool_calls already has index preserved",
			data: []byte(`{"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"foo","arguments":""},"index":5}]}}]}`),
			want: []byte(`{"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"foo","arguments":""},"index":5}]}}]}`),
		},
		{
			name: "invalid JSON passes through unchanged",
			data: []byte(`{invalid json`),
			want: []byte(`{invalid json`),
		},
		{
			name: "real-world Gemini response with thought_signature preserves existing index",
			data: []byte(`{"choices":[{"delta":{"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"execute_command","arguments":"{\"command\":\"ls\"}"},"thought_signature":"dGVzdA==","index":0}]}}]}`),
			want: []byte(`{"choices":[{"delta":{"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"execute_command","arguments":"{\"command\":\"ls\"}"},"thought_signature":"dGVzdA==","index":0}]}}]}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := transformer.TransformSSEChunk(tt.data)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			trimmed := bytes.TrimSpace(tt.data)
			if len(trimmed) == 0 || !bytes.HasPrefix(trimmed, []byte("{")) {
				assertEqualBytes(t, got, tt.want)
			} else {
				var check interface{}
				if json.Unmarshal(tt.data, &check) != nil {
					assertEqualBytes(t, got, tt.want)
				} else {
					assertEqualJSON(t, got, tt.want)
				}
			}
		})
	}
}

func TestGeminiTransformer_OnExtraContent(t *testing.T) {
	var calledWithToolCallID string
	var calledWithExtraContent interface{}

	transformer := &GeminiTransformer{
		OnExtraContent: func(toolCallID string, extraContent interface{}) {
			calledWithToolCallID = toolCallID
			calledWithExtraContent = extraContent
		},
	}

	data := []byte(`{"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"foo","arguments":""},"extra_content":{"reasoning":"because"}}]}}]}`)
	_, err := transformer.TransformSSEChunk(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calledWithToolCallID != "call_1" {
		t.Errorf("toolCallID = %q, want %q", calledWithToolCallID, "call_1")
	}

	extraJSON, _ := json.Marshal(calledWithExtraContent)
	wantExtra := `{"reasoning":"because"}`
	if string(extraJSON) != wantExtra {
		t.Errorf("extraContent = %s, want %s", extraJSON, wantExtra)
	}
}

func TestGeminiTransformer_OnExtraContent_Nil(t *testing.T) {
	transformer := &GeminiTransformer{}

	data := []byte(`{"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"foo","arguments":""},"extra_content":{"reasoning":"because"}}]}}]}`)
	_, err := transformer.TransformSSEChunk(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestToInt(t *testing.T) {
	tests := []struct {
		name string
		v    interface{}
		want int
	}{
		{name: "float64 to int truncation", v: float64(3.9), want: 3},
		{name: "float64 zero", v: float64(0), want: 0},
		{name: "string returns 0", v: "hello", want: 0},
		{name: "nil returns 0", v: nil, want: 0},
		{name: "bool returns 0", v: true, want: 0},
		{name: "int pass through", v: 42, want: 42},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ToInt(tt.v)
			if got != tt.want {
				t.Errorf("ToInt(%v) = %d, want %d", tt.v, got, tt.want)
			}
		})
	}
}
