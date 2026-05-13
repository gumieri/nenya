package providers

import (
	"context"
	"encoding/json"
	"testing"
)

func TestOllamaTransformer_TransformToolCall(t *testing.T) {
	transformer := newOllamaTransformer(nil)

	input := `{"name": "git_status", "arguments": {"includeUntracked": true, "path": "/absolute/path/to/repo"}}`
	got, err := transformer.TransformSSEChunk(context.Background(), []byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(got, &result); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	id, ok := result["id"].(string)
	if !ok || id == "" {
		t.Error("expected non-empty id")
	}
	if obj, ok := result["object"].(string); !ok || obj != "chat.completion.chunk" {
		t.Errorf("expected object 'chat.completion.chunk', got %q", obj)
	}

	choices, ok := result["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatal("expected non-empty choices")
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		t.Fatal("choice is not a map")
	}

	delta, ok := choice["delta"].(map[string]any)
	if !ok {
		t.Fatal("expected delta in choice")
	}
	tcs, ok := delta["tool_calls"].([]any)
	if !ok || len(tcs) == 0 {
		t.Fatal("expected non-empty tool_calls")
	}
	tc, ok := tcs[0].(map[string]any)
	if !ok {
		t.Fatal("tool_call is not a map")
	}
	if n, ok := tc["function"].(map[string]any)["name"].(string); !ok || n != "git_status" {
		t.Errorf("expected function name 'git_status', got %q", n)
	}
	if args, ok := tc["function"].(map[string]any)["arguments"].(string); !ok {
		t.Error("expected arguments as string")
	} else if args == "" {
		t.Error("expected non-empty arguments")
	}
}

func TestOllamaTransformer_TransformNullArguments(t *testing.T) {
	transformer := newOllamaTransformer(nil)

	input := `{"name": "some_tool", "arguments": null}`
	got, err := transformer.TransformSSEChunk(context.Background(), []byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(got, &result); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	args := result["choices"].([]any)[0].(map[string]any)
	delta := args["delta"].(map[string]any)
	tcs := delta["tool_calls"].([]any)
	if argsStr := tcs[0].(map[string]any)["function"].(map[string]any)["arguments"].(string); argsStr != "{}" {
		t.Errorf("expected arguments '{}', got %q", argsStr)
	}
}

func TestOllamaTransformer_PassthroughNonToolCall(t *testing.T) {
	transformer := newOllamaTransformer(nil)

	input := `{"content": "hello world"}`
	got, err := transformer.TransformSSEChunk(context.Background(), []byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(got) != input {
		t.Errorf("expected passthrough, got %q", string(got))
	}
}

func TestOllamaTransformer_IncrementsIndex(t *testing.T) {
	transformer := newOllamaTransformer(nil)

	first, _ := transformer.TransformSSEChunk(context.Background(), []byte(`{"name": "tool_one", "arguments": {}}`))
	second, _ := transformer.TransformSSEChunk(context.Background(), []byte(`{"name": "tool_two", "arguments": {}}`))

	var f, s map[string]any
	json.Unmarshal(first, &f)
	json.Unmarshal(second, &s)

	idxFirst := f["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["index"].(float64)
	idxSecond := s["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["index"].(float64)

	if idxSecond != idxFirst+1 {
		t.Errorf("expected index %v, got %v", idxFirst+1, idxSecond)
	}
}

func TestOllamaTransformer_EmptyObjectPassthrough(t *testing.T) {
	transformer := newOllamaTransformer(nil)

	got, err := transformer.TransformSSEChunk(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{}` {
		t.Errorf("expected passthrough for empty object, got %q", string(got))
	}
}

func TestOllamaTransformer_EmptyArgumentsMap(t *testing.T) {
	transformer := newOllamaTransformer(nil)

	input := `{"name": "some_tool", "arguments": {}}`
	got, err := transformer.TransformSSEChunk(context.Background(), []byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	json.Unmarshal(got, &result)

	args := result["choices"].([]any)[0].(map[string]any)
	delta := args["delta"].(map[string]any)
	tcs := delta["tool_calls"].([]any)
	if argsStr := tcs[0].(map[string]any)["function"].(map[string]any)["arguments"].(string); argsStr != "{}" {
		t.Errorf("expected arguments '{}', got %q", argsStr)
	}
}
