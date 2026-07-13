package routing

import (
	"testing"
)

func TestStripBareReasoningField_WithReasoningEffort(t *testing.T) {
	deps := defaultSanitizeDeps()
	payload := map[string]interface{}{
		"model":            "o1-preview",
		"reasoning":        map[string]interface{}{"max_tokens": 10000},
		"reasoning_effort": "medium",
	}
	stripBareReasoningField(deps, payload, "o1-preview")
	if _, has := payload["reasoning"]; has {
		t.Error("expected bare reasoning field to be stripped when reasoning_effort is present")
	}
	if _, has := payload["reasoning_effort"]; !has {
		t.Error("expected reasoning_effort to be preserved")
	}
}

func TestStripBareReasoningField_NonReasoningModel(t *testing.T) {
	deps := defaultSanitizeDeps()
	payload := map[string]interface{}{
		"model":     "gpt-4-turbo",
		"reasoning": map[string]interface{}{"max_tokens": 10000},
	}
	stripBareReasoningField(deps, payload, "gpt-4-turbo")
	if _, has := payload["reasoning"]; has {
		t.Error("expected bare reasoning field to be stripped for non-reasoning model")
	}
}

func TestStripBareReasoningField_PreserveForReasoningModel(t *testing.T) {
	deps := defaultSanitizeDeps()
	payload := map[string]interface{}{
		"model":     "o1-preview",
		"reasoning": map[string]interface{}{"max_tokens": 10000},
	}
	stripBareReasoningField(deps, payload, "o1-preview")
	if _, has := payload["reasoning"]; !has {
		t.Error("expected bare reasoning field to be preserved for reasoning-capable model when no reasoning_effort")
	}
}

func TestStripBareReasoningField_NoReasoningField(t *testing.T) {
	deps := defaultSanitizeDeps()
	payload := map[string]interface{}{
		"model":            "o1-preview",
		"reasoning_effort": "medium",
	}
	stripBareReasoningField(deps, payload, "o1-preview")
	if _, has := payload["reasoning"]; has {
		t.Error("expected no reasoning field to exist (never present)")
	}
}

func TestStripBareReasoningField_EmptyReasoningObject(t *testing.T) {
	deps := defaultSanitizeDeps()
	payload := map[string]interface{}{
		"model":            "o1-preview",
		"reasoning":        map[string]interface{}{},
		"reasoning_effort": "low",
	}
	stripBareReasoningField(deps, payload, "o1-preview")
	if _, has := payload["reasoning"]; has {
		t.Error("expected empty reasoning field to be stripped when reasoning_effort is present")
	}
}

func TestSanitizePayload_IntegratesBareReasoningStripping(t *testing.T) {
	deps := defaultSanitizeDeps()
	payload := map[string]interface{}{
		"model":            "o1-preview",
		"reasoning":        map[string]interface{}{"max_tokens": 10000},
		"reasoning_effort": "high",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}
	SanitizePayload(deps, payload, "o1-preview")
	if _, has := payload["reasoning"]; has {
		t.Error("expected SanitizePayload to strip bare reasoning field")
	}
	if _, has := payload["reasoning_effort"]; !has {
		t.Error("expected reasoning_effort to be preserved in SanitizePayload")
	}
}

func TestStripBareReasoningField_NilPayload(t *testing.T) {
	deps := defaultSanitizeDeps()
	stripBareReasoningField(deps, nil, "gpt-4-turbo")
}

func TestStripBareReasoningField_EmptyPayload(t *testing.T) {
	deps := defaultSanitizeDeps()
	payload := map[string]interface{}{}
	stripBareReasoningField(deps, payload, "gpt-4-turbo")
	if len(payload) != 0 {
		t.Error("expected empty payload to remain empty")
	}
}

func TestStripBareReasoningField_NonMapReasoning(t *testing.T) {
	deps := defaultSanitizeDeps()
	payload := map[string]interface{}{
		"model":     "gpt-4-turbo",
		"reasoning": "invalid",
	}
	stripBareReasoningField(deps, payload, "gpt-4-turbo")
	if _, has := payload["reasoning"]; has {
		t.Error("expected non-map reasoning field to be stripped even when not a map")
	}
}

func TestStripBareReasoningField_NoSideEffects(t *testing.T) {
	deps := defaultSanitizeDeps()
	payload := map[string]interface{}{
		"model":            "gpt-4-turbo",
		"reasoning":        map[string]interface{}{"max_tokens": 10000},
		"temperature":      0.7,
		"max_tokens":       4096,
		"top_p":            0.9,
		"reasoning_effort": "low",
	}
	stripBareReasoningField(deps, payload, "gpt-4-turbo")
	if _, has := payload["reasoning"]; has {
		t.Error("expected reasoning to be stripped")
	}
	if _, has := payload["reasoning_effort"]; !has {
		t.Error("expected reasoning_effort to be preserved")
	}
	if payload["temperature"] != 0.7 {
		t.Error("expected temperature to be unchanged")
	}
	if payload["max_tokens"] != 4096 {
		t.Error("expected max_tokens to be unchanged")
	}
	if payload["top_p"] != 0.9 {
		t.Error("expected top_p to be unchanged")
	}
}
