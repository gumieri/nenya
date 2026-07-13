package providers

import (
	"testing"
)

func TestXAI_InjectReasoningEffortForReasoningModel(t *testing.T) {
	deps := &SanitizeDeps{}
	deps.SupportsReasoning = func(model string) bool {
		return model == "grok-4.3"
	}

	payload := map[string]interface{}{
		"model": "grok-4.3",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}

	XaiSanitize(deps, payload)

	effort, ok := payload["reasoning_effort"].(string)
	if !ok {
		t.Fatal("expected reasoning_effort to be injected")
	}
	if effort != "medium" {
		t.Fatalf("expected reasoning_effort to be medium, got %q", effort)
	}
}

func TestXAI_SkipReasoningEffortForNonReasoningModel(t *testing.T) {
	deps := &SanitizeDeps{}
	deps.SupportsReasoning = func(model string) bool {
		return false
	}

	payload := map[string]interface{}{
		"model": "grok-4",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}

	XaiSanitize(deps, payload)

	if _, hasEffort := payload["reasoning_effort"]; hasEffort {
		t.Error("expected reasoning_effort NOT to be injected for non-reasoning model")
	}
}

func TestXAI_RespectsClientReasoningEffort(t *testing.T) {
	deps := &SanitizeDeps{}
	deps.SupportsReasoning = func(model string) bool {
		return model == "grok-4.3"
	}

	payload := map[string]interface{}{
		"model":            "grok-4.3",
		"reasoning_effort": "high",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}

	XaiSanitize(deps, payload)

	effort, ok := payload["reasoning_effort"].(string)
	if !ok {
		t.Fatal("expected reasoning_effort to be preserved")
	}
	if effort != "high" {
		t.Fatalf("expected reasoning_effort to remain high, got %q", effort)
	}
}

func TestXAI_NoReasoningWhenSupportsReasoningNil(t *testing.T) {
	deps := &SanitizeDeps{}
	deps.SupportsReasoning = nil

	payload := map[string]interface{}{
		"model": "grok-4.3",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}

	XaiSanitize(deps, payload)

	if _, hasEffort := payload["reasoning_effort"]; hasEffort {
		t.Error("expected reasoning_effort NOT to be injected when SupportsReasoning is nil")
	}
}

func TestXAI_NoReasoningWithoutModel(t *testing.T) {
	deps := &SanitizeDeps{}
	deps.SupportsReasoning = func(model string) bool {
		return true
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}

	XaiSanitize(deps, payload)

	if _, hasEffort := payload["reasoning_effort"]; hasEffort {
		t.Error("expected reasoning_effort NOT to be injected when model is missing")
	}
}

func TestXAI_NoReasoningWithEmptyModel(t *testing.T) {
	deps := &SanitizeDeps{}
	deps.SupportsReasoning = func(model string) bool {
		return true
	}

	payload := map[string]interface{}{
		"model":    "",
		"messages": []interface{}{},
	}

	XaiSanitize(deps, payload)

	if _, hasEffort := payload["reasoning_effort"]; hasEffort {
		t.Error("expected reasoning_effort NOT to be injected when model is empty")
	}
}
