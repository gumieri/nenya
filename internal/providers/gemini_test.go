package providers

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"nenya/internal/infra"
)

func geminiDeps() *SanitizeDeps {
	return &SanitizeDeps{
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
		ExtractContentText: func(msg map[string]interface{}) string {
			if c, ok := msg["content"].(string); ok {
				return c
			}
			return ""
		},
	}
}

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

func TestGeminiModelMap(t *testing.T) {
	if len(GeminiModelMap) == 0 {
		t.Fatal("GeminiModelMap should not be empty")
	}
	if GeminiModelMap["gemini-flash"] != "gemini-2.5-flash" {
		t.Fatalf("expected gemini-flash -> gemini-2.5-flash, got %s", GeminiModelMap["gemini-flash"])
	}
}

func TestGemini_NormalToolCallsNoStripping(t *testing.T) {
	deps := geminiDeps()

	toolCall := map[string]interface{}{
		"id":            "tc-1",
		"type":          "function",
		"extra_content": "thought-sig-abc",
		"function":      map[string]interface{}{"name": "read_file"},
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":       "assistant",
				"content":    "Let me read that file.",
				"tool_calls": []interface{}{toolCall},
			},
			map[string]interface{}{
				"role":         "tool",
				"tool_call_id": "tc-1",
				"content":      "file contents here",
			},
		},
	}

	geminiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	assistant := msgs[0].(map[string]interface{})
	tcs := assistant["tool_calls"].([]interface{})
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(tcs))
	}

	tool := msgs[1].(map[string]interface{})
	if tool["name"] != "read_file" {
		t.Errorf("expected function name 'read_file' injected on tool message, got %v", tool["name"])
	}
}

func TestGemini_OrphanedToolCallsStripped(t *testing.T) {
	deps := geminiDeps()

	toolCallOrphan := map[string]interface{}{
		"id":       "tc-orphan",
		"type":     "function",
		"function": map[string]interface{}{"name": "bad_func"},
	}

	toolCallValid := map[string]interface{}{
		"id":            "tc-valid",
		"type":          "function",
		"extra_content": "thought-sig-valid",
		"function":      map[string]interface{}{"name": "good_func"},
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":       "assistant",
				"content":    "Let me do things.",
				"tool_calls": []interface{}{toolCallOrphan, toolCallValid},
			},
			map[string]interface{}{
				"role":         "tool",
				"tool_call_id": "tc-orphan",
				"content":      "orphan response",
			},
			map[string]interface{}{
				"role":         "tool",
				"tool_call_id": "tc-valid",
				"content":      "valid response",
			},
		},
	}

	geminiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (orphaned pair removed), got %d", len(msgs))
	}

	assistant := msgs[0].(map[string]interface{})
	tcs := assistant["tool_calls"].([]interface{})
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call remaining, got %d", len(tcs))
	}
	tc := tcs[0].(map[string]interface{})
	if tc["id"] != "tc-valid" {
		t.Errorf("expected tc-valid remaining, got %v", tc["id"])
	}

	tool := msgs[1].(map[string]interface{})
	if tool["tool_call_id"] != "tc-valid" {
		t.Errorf("expected tc-valid tool response, got %v", tool["tool_call_id"])
	}
}

func TestGemini_OrphanedToolResponsesRemoved(t *testing.T) {
	deps := geminiDeps()

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "assistant",
				"content": "I'll call a function.",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":       "tc-no-extra",
						"type":     "function",
						"function": map[string]interface{}{"name": "orphan_func"},
					},
				},
			},
			map[string]interface{}{
				"role":         "tool",
				"tool_call_id": "tc-no-extra",
				"content":      "this should be removed",
			},
			map[string]interface{}{
				"role":    "user",
				"content": "continue",
			},
		},
	}

	geminiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (assistant with content stays, orphan tool removed), got %d", len(msgs))
	}
	if msgs[0].(map[string]interface{})["role"] != "assistant" {
		t.Error("expected assistant message first (it has content)")
	}
	if msgs[1].(map[string]interface{})["role"] != "user" {
		t.Error("expected user message second")
	}
}

func TestGemini_FunctionNameInjection(t *testing.T) {
	deps := geminiDeps()

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":            "tc-1",
						"type":          "function",
						"extra_content": "sig",
						"function":      map[string]interface{}{"name": "write_file"},
					},
				},
			},
			map[string]interface{}{
				"role":         "tool",
				"tool_call_id": "tc-1",
				"content":      "done",
			},
		},
	}

	geminiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	tool := msgs[1].(map[string]interface{})
	if tool["name"] != "write_file" {
		t.Errorf("expected function name 'write_file' injected, got %v", tool["name"])
	}
}

func TestGemini_CachedThoughtSignature(t *testing.T) {
	cache := infra.NewThoughtSignatureCache(100, 10*time.Minute)
	cache.Store("tc-cached", "cached-thought-sig")

	deps := geminiDeps()
	deps.ThoughtSigCache = cache

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":       "tc-cached",
						"type":     "function",
						"function": map[string]interface{}{"name": "search"},
					},
				},
			},
			map[string]interface{}{
				"role":         "tool",
				"tool_call_id": "tc-cached",
				"content":      "results",
			},
		},
	}

	geminiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (not orphaned since cached), got %d", len(msgs))
	}

	assistant := msgs[0].(map[string]interface{})
	tcs := assistant["tool_calls"].([]interface{})
	tc := tcs[0].(map[string]interface{})
	if tc["extra_content"] != "cached-thought-sig" {
		t.Errorf("expected cached thought_signature injected, got %v", tc["extra_content"])
	}
}

func TestGemini_EmptyAssistantAfterStripping(t *testing.T) {
	deps := geminiDeps()

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": "hello",
			},
			map[string]interface{}{
				"role": "assistant",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":       "tc-orphan",
						"type":     "function",
						"function": map[string]interface{}{"name": "no_extra"},
					},
				},
			},
			map[string]interface{}{
				"role":         "tool",
				"tool_call_id": "tc-orphan",
				"content":      "orphan response",
			},
		},
	}

	geminiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message (empty assistant removed + orphan tool removed), got %d", len(msgs))
	}
	if msgs[0].(map[string]interface{})["role"] != "user" {
		t.Error("expected only user message remaining")
	}
}

func TestGemini_MessagesWithoutToolCallsUnchanged(t *testing.T) {
	deps := geminiDeps()

	originalMessages := []interface{}{
		map[string]interface{}{"role": "system", "content": "You are helpful."},
		map[string]interface{}{"role": "user", "content": "hello"},
		map[string]interface{}{"role": "assistant", "content": "hi there"},
		map[string]interface{}{"role": "user", "content": "bye"},
	}

	payload := map[string]interface{}{
		"messages": originalMessages,
	}

	geminiSanitize(deps, payload)

	msgs := payload["messages"].([]interface{})
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages unchanged, got %d", len(msgs))
	}
	for i, m := range msgs {
		if m.(map[string]interface{})["role"] != originalMessages[i].(map[string]interface{})["role"] {
			t.Errorf("message %d role changed", i)
		}
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
