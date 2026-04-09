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
	input := `: this is a comment

data: {"choices":[{"delta":{"content":"hi"}}]}

: another comment
`
	reader := NewSSETransformingReader(strings.NewReader(input), &mockTransformer{})
	var buf bytes.Buffer
	_, err := io.Copy(&buf, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, ": this is a comment\n") {
		t.Fatal("comment line not preserved")
	}
	if !strings.Contains(output, ": another comment\n") {
		t.Fatal("second comment not preserved")
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

func TestToInt_Float64(t *testing.T) {
	if v := ToInt(float64(42.7)); v != 42 {
		t.Fatalf("expected 42, got %d", v)
	}
}

func TestToInt_Float64Zero(t *testing.T) {
	if v := ToInt(float64(0)); v != 0 {
		t.Fatalf("expected 0, got %d", v)
	}
}

func TestToInt_Int(t *testing.T) {
	if v := ToInt(7); v != 7 {
		t.Fatalf("expected 7, got %d", v)
	}
}

func TestToInt_String(t *testing.T) {
	if v := ToInt("not a number"); v != 0 {
		t.Fatalf("expected 0, got %d", v)
	}
}

func TestToInt_Nil(t *testing.T) {
	if v := ToInt(nil); v != 0 {
		t.Fatalf("expected 0, got %d", v)
	}
}

func TestToInt_Bool(t *testing.T) {
	if v := ToInt(true); v != 0 {
		t.Fatalf("expected 0, got %d", v)
	}
}

func TestExtractUsageFromMap_NonJSON(t *testing.T) {
	fired := false
	r := &SSETransformingReader{
		onUsage: func(_, _, _ int) { fired = true },
	}
	r.extractUsageFromMap(map[string]interface{}{
		"usage": "not a map",
	})
	if fired {
		t.Fatal("callback should not fire for non-map usage")
	}
}

func TestExtractUsageFromMap_MalformedJSON(t *testing.T) {
	fired := false
	r := &SSETransformingReader{
		onUsage: func(_, _, _ int) { fired = true },
	}
	r.extractUsageFromMap(map[string]interface{}{
		"usage": map[string]interface{}{
			"completion_tokens": "bad",
			"prompt_tokens":     "bad",
			"total_tokens":      "bad",
		},
	})
	if fired {
		t.Fatal("callback should not fire for non-numeric token values")
	}
}

func TestExtractUsageFromMap_NoUsageField(t *testing.T) {
	fired := false
	r := &SSETransformingReader{
		onUsage: func(_, _, _ int) { fired = true },
	}
	r.extractUsageFromMap(map[string]interface{}{
		"choices": []interface{}{},
	})
	if fired {
		t.Fatal("callback should not fire when usage field is absent")
	}
}

func TestExtractUsageFromMap_AllZeroUsage(t *testing.T) {
	fired := false
	r := &SSETransformingReader{
		onUsage: func(_, _, _ int) { fired = true },
	}
	r.extractUsageFromMap(map[string]interface{}{
		"usage": map[string]interface{}{
			"completion_tokens": float64(0),
			"prompt_tokens":     float64(0),
			"total_tokens":      float64(0),
		},
	})
	if fired {
		t.Fatal("callback should not fire when all usage values are zero")
	}
}
