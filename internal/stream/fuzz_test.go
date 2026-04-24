package stream

import (
	"encoding/json"
	"testing"
)

func FuzzParseSSEChunk(f *testing.F) {
	f.Add([]byte(`{"choices":[{"delta":{"content":"hello"}}]}`))
	f.Add([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":null}]}}]}`))
	f.Add([]byte(``))
	f.Fuzz(func(t *testing.T, data []byte) {
		result := ParseSSEChunk(data)
		if result == nil {
			return
		}
		_, _ = json.Marshal(result)
	})
}

func FuzzExtractDeltaContentFromMap(f *testing.F) {
	f.Add([]byte(`{"choices":[{"delta":{"content":"test"}}]}`))
	f.Add([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0}]}}]}`))
	f.Add([]byte(`{"choices":[]}`))
	f.Fuzz(func(t *testing.T, jsonBytes []byte) {
		var chunk map[string]interface{}
		if err := json.Unmarshal(jsonBytes, &chunk); err != nil {
			return
		}
		result := ExtractDeltaContentFromMap(chunk)
		if len(result) > 10000 {
			t.Errorf("unexpectedly long content: %d bytes", len(result))
		}
	})
}

func FuzzReplaceDeltaContentMap(f *testing.F) {
	f.Add([]byte(`{"choices":[{"delta":{"content":"old"}}]}`), "new content")
	f.Add([]byte(`{"choices":[{"delta":{}}]}`), "")
	f.Fuzz(func(t *testing.T, jsonBytes []byte, newContent string) {
		var chunk map[string]interface{}
		if err := json.Unmarshal(jsonBytes, &chunk); err != nil {
			return
		}
		result := ReplaceDeltaContentMap(chunk, newContent)
		marshaled, err := json.Marshal(result)
		if err != nil {
			t.Errorf("result is not valid JSON after replacement: %v", err)
		}
		if len(marshaled) > 10*1024*1024 {
			t.Errorf("result unreasonably large: %d bytes", len(marshaled))
		}
	})
}

func FuzzExtractDeltaContent(f *testing.F) {
	f.Add([]byte(`{"choices":[{"delta":{"content":"hello"}}]}`))
	f.Add([]byte(`{"choices":[]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		result := ExtractDeltaContent(data)
		if len(result) > 10000 {
			t.Errorf("unexpectedly long content: %d bytes", len(result))
		}
	})
}

func FuzzNormalizeToolCalls(f *testing.F) {
	f.Add([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":null,"function":{"name":"read_file","arguments":"{}"}}]}}]}`))
	f.Add([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_0","function":null}]}}]}`))
	f.Add([]byte(`{"choices":[{"delta":{"content":"hello"}}]}`))
	f.Fuzz(func(t *testing.T, jsonBytes []byte) {
		var chunk map[string]interface{}
		if err := json.Unmarshal(jsonBytes, &chunk); err != nil {
			return
		}
		state := newToolCallState()
		mutated := normalizeToolCalls(chunk, &state)
		if mutated {
			_, err := json.Marshal(chunk)
			if err != nil {
				t.Errorf("mutated chunk is not valid JSON: %v", err)
			}
		}
	})
}
