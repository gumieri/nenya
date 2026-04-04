package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestApplyWindowCompactionDisabled(t *testing.T) {
	cfg := Config{
		Window: WindowConfig{Enabled: false},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	messages := []interface{}{
		map[string]interface{}{"role": "user", "content": "hello"},
		map[string]interface{}{"role": "assistant", "content": "world"},
	}

	payload := map[string]interface{}{"messages": messages}
	mutated, err := g.applyWindowCompaction(context.Background(), payload, messages, 1000, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mutated {
		t.Error("expected no mutation when disabled")
	}
	if len(messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(messages))
	}
}

func TestApplyWindowCompactionBelowThreshold(t *testing.T) {
	cfg := Config{
		Window: WindowConfig{
			Enabled:        true,
			ActiveMessages: 2,
			TriggerRatio:   0.8,
			MaxContext:     128000,
			Mode:           "truncate",
		},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	messages := []interface{}{
		map[string]interface{}{"role": "user", "content": "hello"},
		map[string]interface{}{"role": "assistant", "content": "world"},
	}

	payload := map[string]interface{}{"messages": messages}
	mutated, err := g.applyWindowCompaction(context.Background(), payload, messages, 100, 128000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mutated {
		t.Error("expected no mutation below threshold")
	}
}

func TestApplyWindowCompactionNotEnoughMessages(t *testing.T) {
	cfg := Config{
		Window: WindowConfig{
			Enabled:        true,
			ActiveMessages: 6,
			TriggerRatio:   0.8,
			MaxContext:     100,
			Mode:           "truncate",
		},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	messages := []interface{}{
		map[string]interface{}{"role": "user", "content": "hello"},
		map[string]interface{}{"role": "assistant", "content": "world"},
	}

	payload := map[string]interface{}{"messages": messages}
	mutated, err := g.applyWindowCompaction(context.Background(), payload, messages, 500, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mutated {
		t.Error("expected no mutation with fewer messages than active_messages")
	}
}

func TestApplyWindowCompactionTruncateMode(t *testing.T) {
	cfg := Config{
		Window: WindowConfig{
			Enabled:         true,
			ActiveMessages:  2,
			TriggerRatio:    0.8,
			MaxContext:      100,
			Mode:            "truncate",
			SummaryMaxRunes: 200,
		},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	messages := []interface{}{
		map[string]interface{}{"role": "user", "content": strings.Repeat("old history ", 100)},
		map[string]interface{}{"role": "assistant", "content": strings.Repeat("old response ", 100)},
		map[string]interface{}{"role": "user", "content": strings.Repeat("recent history ", 100)},
		map[string]interface{}{"role": "assistant", "content": "keep this"},
	}

	payload := map[string]interface{}{"messages": messages}
	mutated, err := g.applyWindowCompaction(context.Background(), payload, messages, 500, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mutated {
		t.Fatal("expected mutation")
	}

	resultMessages := payload["messages"].([]interface{})
	if len(resultMessages) != 3 {
		t.Fatalf("expected 3 messages (summary + 2 active), got %d", len(resultMessages))
	}

	summaryMsg := resultMessages[0].(map[string]interface{})
	if role, _ := summaryMsg["role"].(string); role != "system" {
		t.Errorf("expected summary role 'system', got %q", role)
	}
	content, _ := summaryMsg["content"].(string)
	if !strings.Contains(content, "[Nenya Window Summary") {
		t.Error("expected window summary marker in content")
	}
	if !strings.Contains(content, "2 messages compacted") {
		t.Error("expected compacted message count in content")
	}

	lastMsg := resultMessages[2].(map[string]interface{})
	if lastContent, _ := lastMsg["content"].(string); lastContent != "keep this" {
		t.Errorf("expected last active message preserved, got %q", lastContent)
	}
}

func TestTruncateHistory(t *testing.T) {
	cfg := Config{
		Window: WindowConfig{SummaryMaxRunes: 100},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	short := "short text"
	result := g.truncateHistory(short)
	if result != short {
		t.Errorf("expected no truncation, got %q", result)
	}

	long := strings.Repeat("abcdefgh ", 100)
	result = g.truncateHistory(long)
	if utf8.RuneCountInString(result) > 100+len("[NENYA: HISTORY TRUNCATED]")+10 {
		t.Errorf("expected truncation to ~100 runes, got %d", utf8.RuneCountInString(result))
	}
	if !strings.Contains(result, "[NENYA: HISTORY TRUNCATED]") {
		t.Error("expected truncation marker")
	}
}

func TestSerializeMessages(t *testing.T) {
	cfg := Config{}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	messages := []interface{}{
		map[string]interface{}{"role": "user", "content": "hello"},
		map[string]interface{}{"role": "assistant", "content": "world"},
	}

	result := g.serializeMessages(messages)
	expected := "user:\nhello\n\nassistant:\nworld\n\n"
	if result != expected {
		t.Errorf("got %q, want %q", result, expected)
	}
}

func TestSerializeMessagesSkipsEmpty(t *testing.T) {
	cfg := Config{}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	messages := []interface{}{
		map[string]interface{}{"role": "user", "content": "hello"},
		map[string]interface{}{"role": "assistant"},
		map[string]interface{}{"role": "tool", "content": ""},
		nil,
	}

	result := g.serializeMessages(messages)
	expected := "user:\nhello\n\n"
	if result != expected {
		t.Errorf("got %q, want %q", result, expected)
	}
}

func TestApplyWindowCompactionZeroMaxContext(t *testing.T) {
	cfg := Config{
		Window: WindowConfig{
			Enabled:        true,
			ActiveMessages: 2,
			TriggerRatio:   0.8,
			MaxContext:     0,
			Mode:           "truncate",
		},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	messages := []interface{}{
		map[string]interface{}{"role": "user", "content": "hello"},
		map[string]interface{}{"role": "assistant", "content": "world"},
	}

	payload := map[string]interface{}{"messages": messages}
	mutated, err := g.applyWindowCompaction(context.Background(), payload, messages, 500, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mutated {
		t.Error("expected no mutation when both maxContext and window.max_context are 0")
	}
}

func TestApplyWindowCompactionUnknownMode(t *testing.T) {
	cfg := Config{
		Window: WindowConfig{
			Enabled:        true,
			ActiveMessages: 2,
			TriggerRatio:   0.8,
			MaxContext:     100,
			Mode:           "invalid_mode",
		},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	messages := []interface{}{
		map[string]interface{}{"role": "user", "content": strings.Repeat("old ", 100)},
		map[string]interface{}{"role": "assistant", "content": strings.Repeat("old ", 100)},
		map[string]interface{}{"role": "user", "content": "recent"},
		map[string]interface{}{"role": "assistant", "content": "keep"},
	}

	payload := map[string]interface{}{"messages": messages}
	mutated, err := g.applyWindowCompaction(context.Background(), payload, messages, 500, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mutated {
		t.Error("expected no mutation with unknown mode")
	}
}
