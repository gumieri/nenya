package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"nenya/internal/infra"
	"nenya/internal/stream"
)

func newOllamaTransformer(_ *infra.ThoughtSignatureCache) stream.ResponseTransformer {
	return &OllamaTransformer{}
}

// extractArgsString converts a raw arguments value to a JSON string.
// Handles string, map[string]any, and nil types.
func extractArgsString(argsRaw any) string {
	switch a := argsRaw.(type) {
	case string:
		if a != "" {
			return a
		}
	case map[string]any:
		if len(a) > 0 {
			encoded, err := json.Marshal(a)
			if err == nil {
				return string(encoded)
			}
		}
	}
	return ""
}

// OllamaTransformer converts Ollama's raw tool-call JSON format
// ({"name": "...", "arguments": ...}) into OpenAI-compatible SSE chunks
// with tool_calls delta. It maintains state for indexing across chunks.
type OllamaTransformer struct {
	callIdx   int
	idCounter int
}

// TransformSSEChunk converts an Ollama SSE chunk to OpenAI format.
// If the chunk contains a "name" field, it's treated as a tool call
// and transformed. Otherwise, the chunk is passed through unchanged.
func (t *OllamaTransformer) TransformSSEChunk(ctx context.Context, data []byte) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return data, nil
	}

	var chunk map[string]any
	if err := json.Unmarshal(data, &chunk); err != nil {
		return data, nil
	}

	nameRaw, hasName := chunk["name"]
	if !hasName {
		return data, nil
	}
	name, ok := nameRaw.(string)
	if !ok || name == "" {
		return data, nil
	}

	argsStr := "{}"
	argsRaw, hasArgs := chunk["arguments"]
	if !hasArgs || argsRaw == nil {
		// leave as "{}"
	} else if argsStr = extractArgsString(argsRaw); argsStr == "" {
		argsStr = "{}"
	}

	t.idCounter++
	t.callIdx++
	tcID := fmt.Sprintf("call_%d", t.callIdx)

	openaiChunk := map[string]any{
		"id":      "ollama-" + fmt.Sprintf("%d", t.idCounter),
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   chunk["model"],
		"choices": []map[string]any{
			{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []map[string]any{
						{
							"index": t.callIdx - 1,
							"id":    tcID,
							"type":  "function",
							"function": map[string]any{
								"name":      name,
								"arguments": argsStr,
							},
						},
					},
				},
			},
		},
	}

	return json.Marshal(openaiChunk)
}
