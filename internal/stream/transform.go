package stream

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type GeminiTransformer struct {
	OnExtraContent func(toolCallID string, extraContent interface{})
}

func (t *GeminiTransformer) TransformSSEChunk(data []byte) ([]byte, error) {
	if len(data) == 0 || !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return data, nil
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return data, nil
	}

	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if delta, ok := choice["delta"].(map[string]interface{}); ok {
				if toolCalls, ok := delta["tool_calls"].([]interface{}); ok {
					for i, tc := range toolCalls {
						if tcMap, ok := tc.(map[string]interface{}); ok {
							if _, exists := tcMap["index"]; !exists {
								tcMap["index"] = i
							}
							if t.OnExtraContent != nil {
								if tcID, _ := tcMap["id"].(string); tcID != "" {
									if extra, hasExtra := tcMap["extra_content"]; hasExtra {
										t.OnExtraContent(tcID, extra)
									}
								}
							}
						}
					}
				}
			}
		}
	}

	transformed, err := json.Marshal(chunk)
	if err != nil {
		return data, fmt.Errorf("failed to marshal transformed chunk: %v", err)
	}

	return transformed, nil
}
