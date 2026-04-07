package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestGeminiTransformer_TransformSSEChunk(t *testing.T) {
	transformer := &GeminiTransformer{}

	tests := []struct {
		name     string
		input    string
		expected string
		wantErr  bool
	}{
		{
			name:     "empty data passes through",
			input:    "",
			expected: "",
			wantErr:  false,
		},
		{
			name:     "non-JSON passes through",
			input:    "[DONE]",
			expected: "[DONE]",
			wantErr:  false,
		},
		{
			name:     "JSON without tool_calls unchanged",
			input:    `{"choices":[{"delta":{"content":"Hello"}}]}`,
			expected: `{"choices":[{"delta":{"content":"Hello"}}]}`,
			wantErr:  false,
		},
		{
			name:     "Gemini tool_calls without index",
			input:    `{"choices":[{"delta":{"tool_calls":[{"id":"1","type":"function","function":{"name":"read"},"extra_content":{"google":{}}}]}}]}`,
			expected: `{"choices":[{"delta":{"tool_calls":[{"extra_content":{"google":{}},"function":{"name":"read"},"id":"1","index":0,"type":"function"}]}}]}`,
			wantErr:  false,
		},
		{
			name:     "Gemini tool_calls with multiple items",
			input:    `{"choices":[{"delta":{"tool_calls":[{"id":"1","type":"function","function":{"name":"read"},"extra_content":{"google":{}}},{"id":"2","type":"function","function":{"name":"write"},"extra_content":{"google":{}}}]}}]}`,
			expected: `{"choices":[{"delta":{"tool_calls":[{"extra_content":{"google":{}},"function":{"name":"read"},"id":"1","index":0,"type":"function"},{"extra_content":{"google":{}},"function":{"name":"write"},"id":"2","index":1,"type":"function"}]}}]}`,
			wantErr:  false,
		},
		{
			name:     "Gemini tool_calls already has index",
			input:    `{"choices":[{"delta":{"tool_calls":[{"id":"1","type":"function","function":{"name":"read"},"index":0,"extra_content":{"google":{}}}]}}]}`,
			expected: `{"choices":[{"delta":{"tool_calls":[{"extra_content":{"google":{}},"function":{"name":"read"},"id":"1","index":0,"type":"function"}]}}]}`,
			wantErr:  false,
		},
		{
			name:     "invalid JSON passes through",
			input:    `{"invalid: json`,
			expected: `{"invalid: json`,
			wantErr:  false,
		},
		{
			name:     "real example from error",
			input:    `{"choices":[{"delta":{"role":"assistant","tool_calls":[{"extra_content":{"google":{"thought_signature":"EjQKMgG+Pvb7s6anNtqTZtb1XVK5Rf2edMXJQpVr53xZCQ3+7yiWdGctjmyn1GLanJ+jMP9P"}},"function":{"arguments":"{\"filePath\":\"/home/rafael/Projects/git.0ur.uk/nenya/gateway.go\"}","name":"read"},"id":"623rbkd0","type":"function"}]},"index":0}],"created":1774892391,"id":"Z7XKaaO9Msi_qtsPkon5-AY","model":"gemini-3.1-flash-lite-preview","object":"chat.completion.chunk","usage":{"completion_tokens":34,"prompt_tokens":49292,"total_tokens":49326}}`,
			expected: `{"choices":[{"delta":{"role":"assistant","tool_calls":[{"extra_content":{"google":{"thought_signature":"EjQKMgG+Pvb7s6anNtqTZtb1XVK5Rf2edMXJQpVr53xZCQ3+7yiWdGctjmyn1GLanJ+jMP9P"}},"function":{"arguments":"{\"filePath\":\"/home/rafael/Projects/git.0ur.uk/nenya/gateway.go\"}","name":"read"},"id":"623rbkd0","index":0,"type":"function"}]},"index":0}],"created":1774892391,"id":"Z7XKaaO9Msi_qtsPkon5-AY","model":"gemini-3.1-flash-lite-preview","object":"chat.completion.chunk","usage":{"completion_tokens":34,"prompt_tokens":49292,"total_tokens":49326}}`,
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := transformer.TransformSSEChunk([]byte(tt.input))
			if (err != nil) != tt.wantErr {
				t.Errorf("TransformSSEChunk() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			// Compare JSON semantically (allow whitespace differences)
			if tt.input != "" && tt.expected != "" {
				var gotJSON, expectedJSON interface{}
				if err := json.Unmarshal(got, &gotJSON); err == nil {
					if err := json.Unmarshal([]byte(tt.expected), &expectedJSON); err == nil {
						// Compare parsed JSON structures
						gotBytes, _ := json.Marshal(gotJSON)
						expectedBytes, _ := json.Marshal(expectedJSON)
						if string(gotBytes) != string(expectedBytes) {
							t.Errorf("TransformSSEChunk() = %s, want %s", gotBytes, expectedBytes)
						}
					}
				}
			} else if string(got) != tt.expected {
				// For non-JSON, compare directly
				t.Errorf("TransformSSEChunk() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestIsGeminiProvider(t *testing.T) {
	cfg := Config{Providers: builtInProviders()}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	tests := []struct {
		provider string
		expected bool
	}{
		{"gemini", true},
		{"zai", false},
		{"deepseek", false},
		{"groq", false},
		{"together", false},
		{"unknown", false},
	}

	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			got := g.isGeminiProvider(tt.provider)
			if got != tt.expected {
				t.Errorf("isGeminiProvider(%q) = %v, want %v", tt.provider, got, tt.expected)
			}
		})
	}
}

func TestSSETransformingReader_Read(t *testing.T) {
	// Test SSE stream transformation
	transformer := &GeminiTransformer{}

	// Create test input with SSE lines
	input := `data: {"choices":[{"delta":{"tool_calls":[{"id":"1","type":"function","function":{"name":"read"},"extra_content":{"google":{}}}]}}]}
data: {"choices":[{"delta":{"content":"Thinking..."}}]}
data: [DONE]
`

	reader := NewSSETransformingReader(strings.NewReader(input), transformer)

	var output bytes.Buffer
	_, err := io.Copy(&output, reader)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	got := output.String()
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")

	// Check line count (should have 3 non-empty lines + final newline)
	if len(lines) != 3 {
		t.Errorf("Expected 3 lines, got %d: %q", len(lines), got)
	}

	// Parse and check each line
	for i, line := range lines {
		if i == 0 {
			// First line should be transformed JSON
			if !strings.HasPrefix(line, "data: ") {
				t.Errorf("Line 0 missing 'data: ' prefix: %q", line)
			}
			data := strings.TrimPrefix(line, "data: ")
			var parsed map[string]interface{}
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				t.Errorf("Line 0 invalid JSON: %v, data: %q", err, data)
			}
			// Check transformation happened
			if !strings.Contains(data, `"index":0`) {
				t.Error("Transformation did not add index field")
			}
		} else if i == 1 {
			// Second line should be unchanged
			if line != `data: {"choices":[{"delta":{"content":"Thinking..."}}]}` {
				t.Errorf("Line 1 changed unexpectedly: %q", line)
			}
		} else if i == 2 {
			// Third line should be [DONE]
			if line != `data: [DONE]` {
				t.Errorf("Line 2 should be [DONE], got: %q", line)
			}
		}
	}
}

func TestSSETransformingReader_NonSSEJSON(t *testing.T) {
	// Test direct JSON streaming (no "data: " prefix)
	transformer := &GeminiTransformer{}

	input := `{"choices":[{"delta":{"tool_calls":[{"id":"1","type":"function","function":{"name":"read"},"extra_content":{"google":{}}}]}}]}
{"choices":[{"delta":{"content":"Done"}}]}`

	reader := NewSSETransformingReader(strings.NewReader(input), transformer)

	var output bytes.Buffer
	_, err := io.Copy(&output, reader)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	got := output.String()
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")

	if len(lines) != 2 {
		t.Fatalf("Expected 2 lines, got %d: %q", len(lines), got)
	}

	// Check first line was transformed
	var firstLine map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &firstLine); err != nil {
		t.Errorf("First line invalid JSON: %v, line: %q", err, lines[0])
	}

	// Verify transformation
	choices, _ := firstLine["choices"].([]interface{})
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]interface{})
		delta, _ := choice["delta"].(map[string]interface{})
		if toolCalls, ok := delta["tool_calls"].([]interface{}); ok && len(toolCalls) > 0 {
			tc, _ := toolCalls[0].(map[string]interface{})
			if index, ok := tc["index"]; !ok || index != float64(0) {
				t.Error("First line missing index field")
			}
		}
	}

	// Check second line unchanged
	var secondLine map[string]interface{}
	if err := json.Unmarshal([]byte(lines[1]), &secondLine); err != nil {
		t.Errorf("Second line invalid JSON: %v, line: %q", err, lines[1])
	}
	// Second line should have content field
	choices2, _ := secondLine["choices"].([]interface{})
	if len(choices2) > 0 {
		choice2, _ := choices2[0].(map[string]interface{})
		delta2, _ := choice2["delta"].(map[string]interface{})
		if _, ok := delta2["content"]; !ok {
			t.Error("Second line missing content field")
		}
	}
}

func TestSSETransformingReader_EmptyAndComments(t *testing.T) {
	transformer := &GeminiTransformer{}

	// Test with SSE comments and empty lines
	input := `event: message
data: {"choices":[{"delta":{"tool_calls":[{"id":"1","type":"function","function":{"name":"read"},"extra_content":{"google":{}}}]}}]}

: This is a comment
data: [DONE]
`

	reader := NewSSETransformingReader(strings.NewReader(input), transformer)

	var output bytes.Buffer
	_, err := io.Copy(&output, reader)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	got := output.String()

	// Should preserve event line and comment
	if !strings.Contains(got, "event: message") {
		t.Error("Lost SSE event line")
	}
	if !strings.Contains(got, ": This is a comment") {
		t.Error("Lost SSE comment line")
	}
	if !strings.Contains(got, `"index":0`) {
		t.Error("Did not transform tool_calls")
	}
}

func TestGetResponseTransformer(t *testing.T) {
	cfg := Config{Providers: builtInProviders()}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	tests := []struct {
		provider string
		expected bool
	}{
		{"gemini", true},
		{"zai", false},
		{"deepseek", false},
		{"groq", false},
		{"together", false},
	}

	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			transformer := g.getResponseTransformer(tt.provider)
			hasTransformer := transformer != nil
			if hasTransformer != tt.expected {
				t.Errorf("getResponseTransformer(%q) hasTransformer=%v, want %v",
					tt.provider, hasTransformer, tt.expected)
			}
		})
	}
}

func TestSSETransformingReader_OnUsage(t *testing.T) {
	input := `data: {"choices":[{"delta":{"content":"Hello"}}]}
data: {"choices":[],"usage":{"completion_tokens":10,"prompt_tokens":50,"total_tokens":60}}
data: [DONE]
`

	var gotCompletion, gotPrompt, gotTotal int
	reader := NewSSETransformingReader(strings.NewReader(input), nil)
	reader.SetOnUsage(func(completion, prompt, total int) {
		gotCompletion = completion
		gotPrompt = prompt
		gotTotal = total
	})

	var output bytes.Buffer
	if _, err := io.Copy(&output, reader); err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if gotCompletion != 10 {
		t.Errorf("expected completion_tokens=10, got %d", gotCompletion)
	}
	if gotPrompt != 50 {
		t.Errorf("expected prompt_tokens=50, got %d", gotPrompt)
	}
	if gotTotal != 60 {
		t.Errorf("expected total_tokens=60, got %d", gotTotal)
	}
}

func TestSSETransformingReader_OnUsageNotFiredForNoUsage(t *testing.T) {
	input := `data: {"choices":[{"delta":{"content":"Hello"}}]}
data: [DONE]
`

	fired := false
	reader := NewSSETransformingReader(strings.NewReader(input), nil)
	reader.SetOnUsage(func(completion, prompt, total int) {
		fired = true
	})

	var output bytes.Buffer
	if _, err := io.Copy(&output, reader); err != nil {
		t.Fatalf("io.Copy failed: %v", err)
	}

	if fired {
		t.Error("OnUsage should not fire when no usage field is present")
	}
}

func TestToInt(t *testing.T) {
	tests := []struct {
		name  string
		input interface{}
		want  int
	}{
		{"float64", float64(42.7), 42},
		{"float64 zero", float64(0), 0},
		{"string", "not a number", 0},
		{"nil", nil, 0},
		{"bool", true, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toInt(tt.input)
			if got != tt.want {
				t.Errorf("toInt(%v) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestExtractUsageEdgeCases(t *testing.T) {
	t.Run("non-JSON data", func(t *testing.T) {
		r := &SSETransformingReader{}
		r.extractUsage([]byte("not json"))
	})

	t.Run("malformed JSON", func(t *testing.T) {
		r := &SSETransformingReader{}
		r.extractUsage([]byte(`{invalid}`))
	})

	t.Run("no usage field", func(t *testing.T) {
		r := &SSETransformingReader{}
		r.extractUsage([]byte(`{"choices":[]}`))
	})

	t.Run("usage with all zeros", func(t *testing.T) {
		fired := false
		r := &SSETransformingReader{}
		r.onUsage = func(c, p, t int) { fired = true }
		r.extractUsage([]byte(`{"usage":{"completion_tokens":0,"prompt_tokens":0,"total_tokens":0}}`))
		if fired {
			t.Error("onUsage should not fire for all-zero usage")
		}
	})

	t.Run("usage with only completion", func(t *testing.T) {
		fired := false
		var gotC int
		r := &SSETransformingReader{}
		r.onUsage = func(c, p, t int) { fired = true; gotC = c }
		r.extractUsage([]byte(`{"usage":{"completion_tokens":5}}`))
		if !fired {
			t.Error("onUsage should fire when completion_tokens > 0")
		}
		if gotC != 5 {
			t.Errorf("got completion_tokens=%d, want 5", gotC)
		}
	})

	t.Run("whitespace before JSON", func(t *testing.T) {
		fired := false
		r := &SSETransformingReader{}
		r.onUsage = func(c, p, t int) { fired = true }
		r.extractUsage([]byte(`   {"usage":{"completion_tokens":1,"prompt_tokens":1,"total_tokens":2}}`))
		if !fired {
			t.Error("onUsage should fire for valid JSON with leading whitespace")
		}
	})
}
