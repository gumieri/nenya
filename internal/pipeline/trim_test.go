package pipeline

import (
	"strings"
	"testing"

	"nenya/config"
)

func TestTrimPayload_UnderLimit(t *testing.T) {
	cfg := config.ContextConfig{
		TruncationStrategy:     "middle-out",
		TruncationKeepFirstPct: 30,
		TruncationKeepLastPct:  30,
	}
	countTokens := func(s string) int { return len(s) }
	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "s"},
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}
	modified, saved := TrimPayload(nil, payload, 100, countTokens, cfg)
	if modified {
		t.Errorf("expected no modification when under limit")
	}
	if saved != 0 {
		t.Errorf("expected 0 saved tokens, got %d", saved)
	}
}

func TestTrimPayload_RemoveOldest(t *testing.T) {
	cfg := config.ContextConfig{
		TruncationStrategy:     "middle-out",
		TruncationKeepFirstPct: 30,
		TruncationKeepLastPct:  30,
	}
	countTokens := func(s string) int { return len(s) }
	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "s"},
			map[string]interface{}{"role": "user", "content": strings.Repeat("a", 20)},
			map[string]interface{}{"role": "assistant", "content": strings.Repeat("b", 20)},
			map[string]interface{}{"role": "user", "content": strings.Repeat("c", 20)},
			map[string]interface{}{"role": "assistant", "content": strings.Repeat("d", 20)},
			map[string]interface{}{"role": "user", "content": strings.Repeat("e", 20)},
		},
	}
	// Budget fits: s(1) + e(20) + d(20) = 41 > 40, so drops oldest
	modified, _ := TrimPayload(nil, payload, 40, countTokens, cfg)
	if !modified {
		t.Fatal("expected modification")
	}
	messages := payload["messages"].([]interface{})
	if len(messages) != 3 {
		t.Errorf("expected 3 messages (system + 2 newest), got %d", len(messages))
	}
	sysMsg := messages[0].(map[string]interface{})
	if sysMsg["role"] != "system" {
		t.Errorf("first message should be system")
	}
}

func TestTrimPayload_TruncateSingle(t *testing.T) {
	cfg := config.ContextConfig{
		TruncationStrategy:     "middle-out",
		TruncationKeepFirstPct: 30,
		TruncationKeepLastPct:  30,
	}
	countTokens := func(s string) int { return len(s) }
	longContent := strings.Repeat("long message ", 20)
	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "s"},
			map[string]interface{}{"role": "user", "content": longContent},
		},
	}
	modified, _ := TrimPayload(nil, payload, 60, countTokens, cfg)
	if !modified {
		t.Fatal("expected modification")
	}
	messages := payload["messages"].([]interface{})
	if len(messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(messages))
	}
	userMsg := messages[1].(map[string]interface{})
	content, ok := userMsg["content"].(string)
	if !ok {
		t.Fatal("user message should have content")
	}
	if len(content) >= len(longContent) {
		t.Errorf("expected truncated content to be shorter than original")
	}
	if !strings.Contains(content, "NENYA: MASSIVE PAYLOAD TRUNCATED") {
		t.Error("expected truncation marker in content")
	}
}

func TestTrimPayload_SystemPreserved(t *testing.T) {
	cfg := config.ContextConfig{
		TruncationStrategy:     "middle-out",
		TruncationKeepFirstPct: 30,
		TruncationKeepLastPct:  30,
	}
	countTokens := func(s string) int { return len(s) }
	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "sys_prompt"},
			map[string]interface{}{"role": "user", "content": strings.Repeat("x", 20)},
			map[string]interface{}{"role": "assistant", "content": strings.Repeat("y", 20)},
			map[string]interface{}{"role": "user", "content": strings.Repeat("z", 20)},
		},
	}
	modified, _ := TrimPayload(nil, payload, 50, countTokens, cfg)
	if !modified {
		t.Fatal("expected modification")
	}
	messages := payload["messages"].([]interface{})
	sysMsg := messages[0].(map[string]interface{})
	if sysMsg["role"] != "system" {
		t.Errorf("first message should be system")
	}
	content, ok := sysMsg["content"].(string)
	if !ok || content != "sys_prompt" {
		t.Error("system message content should be preserved")
	}
}

func TestTrimPayload_ZeroMessages(t *testing.T) {
	cfg := config.ContextConfig{
		TruncationStrategy:     "middle-out",
		TruncationKeepFirstPct: 30,
		TruncationKeepLastPct:  30,
	}
	countTokens := func(s string) int { return len(s) }
	payload := map[string]interface{}{"messages": []interface{}{}}
	modified, saved := TrimPayload(nil, payload, 100, countTokens, cfg)
	if modified {
		t.Errorf("expected no modification with empty messages")
	}
	if saved != 0 {
		t.Errorf("expected 0 saved tokens, got %d", saved)
	}
}
