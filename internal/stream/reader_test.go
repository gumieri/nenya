package stream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
)

type mockTransformer struct{}

func (m *mockTransformer) TransformSSEChunk(data []byte) ([]byte, error) {
	return data, nil
}

func TestSSETransformingReader_DataLines(t *testing.T) {
	input := `data: {"choices":[{"delta":{"content":"hello"}}]}
 data: {"choices":[{"delta":{"content":" world"}}]}
 data: [DONE]
 `
	reader := NewSSETransformingReader(strings.NewReader(input), &mockTransformer{})
	var buf bytes.Buffer
	_, err := io.Copy(&buf, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "data: ") {
		t.Fatalf("expected SSE data lines, got: %s", output)
	}
	if !strings.Contains(output, "hello") {
		t.Fatal("expected 'hello' in output")
	}
	if !strings.Contains(output, " world") {
		t.Fatal("expected ' world' in output")
	}
	if !strings.Contains(output, "data: [DONE]\n") {
		t.Fatal("expected 'data: [DONE]' preserved")
	}
}

func TestSSETransformingReader_NonSSEJSON(t *testing.T) {
	input := `{"choices":[{"delta":{"content":"raw json"}}]}
`
	reader := NewSSETransformingReader(strings.NewReader(input), &mockTransformer{})
	var buf bytes.Buffer
	_, err := io.Copy(&buf, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	output := buf.String()
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(output), &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	choices := parsed["choices"].([]interface{})
	delta := choices[0].(map[string]interface{})["delta"].(map[string]interface{})
	if delta["content"] != "raw json" {
		t.Fatalf("expected 'raw json', got: %v", delta["content"])
	}
}

func TestSSETransformingReader_EmptyLinesAndComments(t *testing.T) {
	input := `
: this is a comment

data: {"choices":[{"delta":{"content":"test"}}]}

`
	reader := NewSSETransformingReader(strings.NewReader(input), &mockTransformer{})
	var buf bytes.Buffer
	_, err := io.Copy(&buf, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "data: ") {
		t.Fatalf("expected SSE data lines, got: %s", output)
	}
	if !strings.Contains(output, "test") {
		t.Fatal("expected 'test' in output")
	}
	if !strings.Contains(output, ": this is a comment\n") {
		t.Fatal("comment line not preserved")
	}
	if strings.Count(output, "\n\n") < 2 {
		t.Fatalf("empty lines not preserved, got: %q", output)
	}
}

func TestSSETransformingReader_StreamFilterBlock(t *testing.T) {
	blockRe := regexp.MustCompile(`(?i)rm\s+-rf`)
	sf := NewStreamFilter(nil, []*regexp.Regexp{blockRe}, "[REDACTED]", 4096)

	input := `data: {"choices":[{"delta":{"content":"run rm -rf / now"}}]}
`
	reader := NewSSETransformingReader(strings.NewReader(input), &mockTransformer{})
	reader.SetStreamFilter(sf)

	var buf bytes.Buffer
	_, err := io.Copy(&buf, reader)
	if !errors.Is(err, ErrStreamBlocked) {
		t.Fatalf("expected ErrStreamBlocked, got: %v", err)
	}
}

func TestSSETransformingReader_StreamFilterRedact(t *testing.T) {
	secretRe := regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	sf := NewStreamFilter([]*regexp.Regexp{secretRe}, nil, "[SECRET]", 4096)

	input := `data: {"choices":[{"delta":{"content":"key is AKIAIOSFODNN7EXAMPLE end"}}]}
`
	reader := NewSSETransformingReader(strings.NewReader(input), &mockTransformer{})
	reader.SetStreamFilter(sf)

	var buf bytes.Buffer
	_, err := io.Copy(&buf, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	output := buf.String()
	if strings.Contains(output, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatal("secret key not redacted")
	}
	if !strings.Contains(output, "[SECRET]") {
		t.Fatal("expected [SECRET] label in output")
	}
	if strings.Contains(output, "key is") && strings.Contains(output, "end") {
		t.Log("surrounding content preserved correctly")
	}
}

func TestSSETransformingReader_OnUsageCallback(t *testing.T) {
	input := `data: {"choices":[{"delta":{"content":"hi"}}]}
data: {"choices":[],"usage":{"completion_tokens":10,"prompt_tokens":5,"total_tokens":15}}
`
	var gotCompletion, gotPrompt, gotTotal int
	cb := func(completion, prompt, total int) {
		gotCompletion = completion
		gotPrompt = prompt
		gotTotal = total
	}

	reader := NewSSETransformingReader(strings.NewReader(input), &mockTransformer{})
	reader.SetOnUsage(cb)

	var buf bytes.Buffer
	_, err := io.Copy(&buf, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotCompletion != 10 || gotPrompt != 5 || gotTotal != 15 {
		t.Fatalf("expected (10,5,15), got (%d,%d,%d)", gotCompletion, gotPrompt, gotTotal)
	}
}

func TestSSETransformingReader_OnUsageNotFired(t *testing.T) {
	input := `data: {"choices":[{"delta":{"content":"hi"}}]}
data: {"choices":[{"delta":{"content":"bye"}}]}
`
	fired := false
	cb := func(_, _, _ int) {
		fired = true
	}

	reader := NewSSETransformingReader(strings.NewReader(input), &mockTransformer{})
	reader.SetOnUsage(cb)

	var buf bytes.Buffer
	_, err := io.Copy(&buf, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fired {
		t.Fatal("usage callback should not have fired")
	}
}

func TestToInt(t *testing.T) {
	tests := []struct {
		name string
		input interface{}
		want int
	}{
		{"float64 truncated", float64(42.7), 42},
		{"float64 zero", float64(0), 0},
		{"int passthrough", 7, 7},
		{"string returns zero", "not a number", 0},
		{"nil returns zero", nil, 0},
		{"bool returns zero", true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ToInt(tt.input); got != tt.want {
				t.Errorf("ToInt(%v) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestExtractUsageFromMap(t *testing.T) {
	tests := []struct {
		name       string
		chunk      map[string]interface{}
		wantFired  bool
	}{
		{
			"non-map usage field",
			map[string]interface{}{"usage": "not a map"},
			false,
		},
		{
			"malformed token values",
			map[string]interface{}{
				"usage": map[string]interface{}{
					"completion_tokens": "bad",
					"prompt_tokens":     "bad",
					"total_tokens":      "bad",
				},
			},
			false,
		},
		{
			"no usage field",
			map[string]interface{}{"choices": []interface{}{}},
			false,
		},
		{
			"all zero usage",
			map[string]interface{}{
				"usage": map[string]interface{}{
					"completion_tokens": float64(0),
					"prompt_tokens":     float64(0),
					"total_tokens":      float64(0),
				},
			},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fired := false
			r := &SSETransformingReader{
				onUsage: func(_, _, _ int) { fired = true },
			}
			r.extractUsageFromMap(tt.chunk)
			if fired != tt.wantFired {
				t.Errorf("callback fired=%v, want %v", fired, tt.wantFired)
			}
		})
	}
}

func TestNormalizeToolCallIDs(t *testing.T) {
	tests := []struct {
		name       string
		chunk      map[string]interface{}
		wantMutate bool
		wantID     string
	}{
		{
			"null id generates synthetic",
			map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{
						"delta": map[string]interface{}{
							"tool_calls": []interface{}{
								map[string]interface{}{
									"index": float64(0), "id": nil,
									"function": map[string]interface{}{"name": "read_file", "arguments": "{}"},
								},
							},
						},
					},
				},
			},
			true, "call_0",
		},
		{
			"numeric id converts to string",
			map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{
						"delta": map[string]interface{}{
							"tool_calls": []interface{}{
								map[string]interface{}{
									"index": float64(3), "id": float64(42),
									"function": map[string]interface{}{"name": "search", "arguments": "{}"},
								},
							},
						},
					},
				},
			},
			true, "call_3",
		},
		{
			"valid string id unchanged",
			map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{
						"delta": map[string]interface{}{
							"tool_calls": []interface{}{
								map[string]interface{}{
									"index": float64(0), "id": "call_abc123",
									"function": map[string]interface{}{"name": "read_file", "arguments": "{}"},
								},
							},
						},
					},
				},
			},
			false, "call_abc123",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutated := normalizeToolCalls(tt.chunk)
			if mutated != tt.wantMutate {
				t.Fatalf("mutated=%v, want %v", mutated, tt.wantMutate)
			}
			choices := tt.chunk["choices"].([]interface{})
			delta := choices[0].(map[string]interface{})["delta"].(map[string]interface{})
			tcs := delta["tool_calls"].([]interface{})
			gotID := tcs[0].(map[string]interface{})["id"].(string)
			if gotID != tt.wantID {
				t.Fatalf("id=%q, want %q", gotID, tt.wantID)
			}
		})
	}
}

func TestNormalizeToolCallIDs_MissingToolCalls(t *testing.T) {
	chunk := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"delta": map[string]interface{}{"content": "hello"},
			},
		},
	}
	if normalizeToolCalls(chunk) {
		t.Fatal("expected no mutation when no tool_calls")
	}
}

func TestSSETransformingReader_ToolCallIDNormalized(t *testing.T) {
	input := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"read_file","arguments":""}}]}}]}
data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":null,"function":{"name":"read_file","arguments":"{}"}}]}}]}
data: [DONE]
`
	reader := NewSSETransformingReader(strings.NewReader(input), nil)
	var buf bytes.Buffer
	_, err := io.Copy(&buf, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	output := buf.String()
	if strings.Contains(output, `"id":null`) {
		t.Fatalf("null tool call id should be normalized, got: %s", output)
	}
	if !strings.Contains(output, `"id":"call_0"`) {
		t.Fatalf("expected synthetic id call_0, got: %s", output)
	}
}

func TestNormalizeToolCalls(t *testing.T) {
	tests := []struct {
		name         string
		chunk        map[string]interface{}
		wantMutate   bool
		wantTCLen    int
		wantFirstID  string
	}{
		{
			"null function stripped",
			map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{
						"delta": map[string]interface{}{
							"tool_calls": []interface{}{
								map[string]interface{}{
									"index": float64(0), "id": "call_0", "function": nil,
								},
								map[string]interface{}{
									"index": float64(1), "id": "call_1",
									"function": map[string]interface{}{"name": "read_file", "arguments": "{}"},
								},
							},
						},
					},
				},
			},
			true, 1, "call_1",
		},
		{
			"null function.name no args removed",
			map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{
						"delta": map[string]interface{}{
							"tool_calls": []interface{}{
								map[string]interface{}{
									"index": float64(0), "id": "call_0",
									"function": map[string]interface{}{"name": nil, "arguments": nil},
								},
							},
						},
					},
				},
			},
			true, 0, "",
		},
		{
			"null function.name with args preserved",
			map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{
						"delta": map[string]interface{}{
							"tool_calls": []interface{}{
								map[string]interface{}{
									"index": float64(0), "id": "call_0",
									"function": map[string]interface{}{"name": nil, "arguments": `{"path":"test.txt"}`},
								},
							},
						},
					},
				},
			},
			false, 1, "call_0",
		},
		{
			"mixed valid and invalid entries",
			map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{
						"delta": map[string]interface{}{
							"tool_calls": []interface{}{
								map[string]interface{}{
									"index": float64(0), "id": "call_0", "function": nil,
								},
								map[string]interface{}{
									"index": float64(1), "id": "call_1",
									"function": map[string]interface{}{"name": "read_file", "arguments": "{}"},
								},
								map[string]interface{}{
									"index": float64(2), "id": "call_2",
									"function": map[string]interface{}{"name": nil, "arguments": nil},
								},
							},
						},
					},
				},
			},
			true, 1, "call_1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutated := normalizeToolCalls(tt.chunk)
			if mutated != tt.wantMutate {
				t.Fatalf("mutated=%v, want %v", mutated, tt.wantMutate)
			}
			choices := tt.chunk["choices"].([]interface{})
			delta := choices[0].(map[string]interface{})["delta"].(map[string]interface{})
			if tt.wantTCLen == 0 {
				if _, hasTC := delta["tool_calls"]; hasTC {
					t.Fatal("tool_calls should be removed")
				}
				return
			}
			tcs := delta["tool_calls"].([]interface{})
			if len(tcs) != tt.wantTCLen {
				t.Fatalf("tool_calls len=%d, want %d", len(tcs), tt.wantTCLen)
			}
			if tt.wantFirstID != "" {
				gotID := tcs[0].(map[string]interface{})["id"].(string)
				if gotID != tt.wantFirstID {
					t.Fatalf("first id=%q, want %q", gotID, tt.wantFirstID)
				}
			}
		})
	}
}
