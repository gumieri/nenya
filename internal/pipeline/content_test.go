package pipeline

import (
	"strings"
	"testing"
)

func TestWalkMessageText(t *testing.T) {
	noop := func(s string) string { return s }
	redactAll := func(s string) string { return "[REDACTED]" }

	t.Run("no content key", func(t *testing.T) {
		if WalkMessageText(map[string]interface{}{}, noop) {
			t.Error("expected false for no content")
		}
	})

	t.Run("empty string content", func(t *testing.T) {
		if WalkMessageText(map[string]interface{}{"content": ""}, noop) {
			t.Error("expected false for empty content")
		}
	})

	t.Run("string content unchanged", func(t *testing.T) {
		node := map[string]interface{}{"content": "hello"}
		if WalkMessageText(node, noop) {
			t.Error("expected false when nothing changed")
		}
	})

	t.Run("string content redacted", func(t *testing.T) {
		node := map[string]interface{}{"content": "my password is secret"}
		if !WalkMessageText(node, redactAll) {
			t.Fatal("expected true when redacted")
		}
		if node["content"] != "[REDACTED]" {
			t.Errorf("expected [REDACTED], got %v", node["content"])
		}
	})

	t.Run("array content with text parts", func(t *testing.T) {
		node := map[string]interface{}{
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "hello"},
				map[string]interface{}{"type": "image_url", "url": "http://example.com/img.png"},
			},
		}
		if !WalkMessageText(node, redactAll) {
			t.Fatal("expected true when text part redacted")
		}
		parts := node["content"].([]interface{})
		first := parts[0].(map[string]interface{})
		if first["text"] != "[REDACTED]" {
			t.Errorf("expected text part redacted, got %v", first["text"])
		}
		second := parts[1].(map[string]interface{})
		if second["url"] != "http://example.com/img.png" {
			t.Errorf("expected non-text part unchanged, got %v", second["url"])
		}
	})

	t.Run("array content multiple text parts", func(t *testing.T) {
		node := map[string]interface{}{
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "one"},
				map[string]interface{}{"type": "text", "text": "two"},
			},
		}
		if !WalkMessageText(node, redactAll) {
			t.Fatal("expected true when any text part redacted")
		}
		parts := node["content"].([]interface{})
		if parts[0].(map[string]interface{})["text"] != "[REDACTED]" {
			t.Error("expected first text part redacted")
		}
		if parts[1].(map[string]interface{})["text"] != "[REDACTED]" {
			t.Error("expected second text part redacted")
		}
	})

	t.Run("array content non-map items", func(t *testing.T) {
		node := map[string]interface{}{
			"content": []interface{}{"not a map"},
		}
		if WalkMessageText(node, redactAll) {
			t.Error("expected false for non-map items")
		}
	})

	t.Run("array content missing text key", func(t *testing.T) {
		node := map[string]interface{}{
			"content": []interface{}{
				map[string]interface{}{"type": "text"},
			},
		}
		if WalkMessageText(node, redactAll) {
			t.Error("expected false for missing text key")
		}
	})

	t.Run("array content unchanged", func(t *testing.T) {
		node := map[string]interface{}{
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "hello"},
			},
		}
		if WalkMessageText(node, noop) {
			t.Error("expected false when no array text part changed")
		}
	})

	t.Run("non-string non-array content", func(t *testing.T) {
		node := map[string]interface{}{"content": 42}
		if WalkMessageText(node, redactAll) {
			t.Error("expected false for non-string non-array content")
		}
	})

	t.Run("tool_calls arguments redacted", func(t *testing.T) {
		node := map[string]interface{}{
			"tool_calls": []interface{}{
				map[string]interface{}{
					"function": map[string]interface{}{
						"name":      "bash",
						"arguments": `{"command":"export KEY=AKIAIOSFODNN7EXAMPLE"}`,
					},
				},
			},
		}
		if !WalkMessageText(node, func(s string) string {
			return strings.ReplaceAll(s, "AKIAIOSFODNN7EXAMPLE", "[REDACTED]")
		}) {
			t.Fatal("expected true when arguments redacted")
		}
		fnObj := node["tool_calls"].([]interface{})[0].(map[string]interface{})["function"].(map[string]interface{})
		if fnObj["arguments"] != `{"command":"export KEY=[REDACTED]"}` {
			t.Errorf("expected arguments redacted in place, got %v", fnObj["arguments"])
		}
	})

	t.Run("tool_calls arguments reverted when fn breaks JSON", func(t *testing.T) {
		args := `{"command":"export KEY=AKIAIOSFODNN7EXAMPLE"}`
		node := map[string]interface{}{
			"tool_calls": []interface{}{
				map[string]interface{}{
					"function": map[string]interface{}{
						"arguments": args,
					},
				},
			},
		}
		if WalkMessageText(node, redactAll) {
			t.Error("expected false when replacement would break JSON validity")
		}
		if node["tool_calls"].([]interface{})[0].(map[string]interface{})["function"].(map[string]interface{})["arguments"] != args {
			t.Error("expected original arguments preserved")
		}
	})

	t.Run("tool_calls non-string arguments skipped", func(t *testing.T) {
		node := map[string]interface{}{
			"tool_calls": []interface{}{
				map[string]interface{}{
					"function": map[string]interface{}{
						"arguments": map[string]interface{}{"command": "ls"},
					},
				},
				"not a map",
			},
		}
		if WalkMessageText(node, redactAll) {
			t.Error("expected false for non-string arguments")
		}
	})
}
