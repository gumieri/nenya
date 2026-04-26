package config

import (
	"testing"
)

func TestResolveEngineRef_InlineProvider(t *testing.T) {
	providers := map[string]*Provider{
		"ollama": {
			Name:      "ollama",
			URL:       "http://localhost:11434/v1/chat/completions",
			ApiFormat: "ollama",
		},
	}

	ref := EngineRef{
		Provider: "ollama",
		Model:    "qwen2.5-coder",
	}

	targets, err := ResolveEngineRef(ref, nil, providers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].Engine.Provider != "ollama" {
		t.Errorf("expected provider ollama, got %s", targets[0].Engine.Provider)
	}
	if targets[0].Engine.Model != "qwen2.5-coder" {
		t.Errorf("expected model qwen2.5-coder, got %s", targets[0].Engine.Model)
	}
	if targets[0].Provider.URL != "http://localhost:11434/v1/chat/completions" {
		t.Errorf("unexpected URL: %s", targets[0].Provider.URL)
	}
}

func TestResolveEngineRef_InlineProviderNotFound(t *testing.T) {
	ref := EngineRef{
		Provider: "nonexistent",
		Model:    "some-model",
	}

	_, err := ResolveEngineRef(ref, nil, nil)
	if err == nil {
		t.Fatal("expected error for nonexistent provider")
	}
}

func TestResolveEngineRef_AgentRef(t *testing.T) {
	providers := map[string]*Provider{
		"ollama": {
			Name:      "ollama",
			URL:       "http://localhost:11434/v1/chat/completions",
			ApiFormat: "ollama",
		},
	}
	agents := map[string]AgentConfig{
		"security": {
			Models: []AgentModel{
				{Provider: "ollama", Model: "qwen2.5-coder"},
			},
			SystemPrompt: "You are a filter.",
		},
	}

	ref := EngineRef{
		AgentName: "security",
	}

	targets, err := ResolveEngineRef(ref, agents, providers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].Engine.SystemPrompt != "You are a filter." {
		t.Errorf("expected agent system prompt, got %q", targets[0].Engine.SystemPrompt)
	}
}

func TestResolveEngineRef_AgentNotFound(t *testing.T) {
	ref := EngineRef{
		AgentName: "nonexistent",
	}

	_, err := ResolveEngineRef(ref, nil, nil)
	if err == nil {
		t.Fatal("expected error for nonexistent agent")
	}
}

func TestResolveEngineRef_AgentNoModels(t *testing.T) {
	agents := map[string]AgentConfig{
		"empty": {},
	}
	ref := EngineRef{AgentName: "empty"}

	_, err := ResolveEngineRef(ref, agents, nil)
	if err == nil {
		t.Fatal("expected error for agent with no models")
	}
}

func TestResolveEngineRef_InlineSystemPromptOverrides(t *testing.T) {
	providers := map[string]*Provider{
		"ollama": {Name: "ollama", URL: "http://localhost:11434/v1/chat/completions"},
	}
	agents := map[string]AgentConfig{
		"security": {
			Models:       []AgentModel{{Provider: "ollama", Model: "qwen"}},
			SystemPrompt: "Agent prompt",
		},
	}

	ref := EngineRef{
		AgentName:    "security",
		SystemPrompt: "Inline prompt",
	}

	targets, err := ResolveEngineRef(ref, agents, providers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if targets[0].Engine.SystemPrompt != "Inline prompt" {
		t.Errorf("expected inline prompt to override agent prompt, got %q", targets[0].Engine.SystemPrompt)
	}
}

func TestResolveEngineRef_TimeoutFromProvider(t *testing.T) {
	providers := map[string]*Provider{
		"ollama": {
			Name:           "ollama",
			URL:            "http://localhost:11434/v1/chat/completions",
			TimeoutSeconds: 30,
		},
	}

	ref := EngineRef{
		Provider: "ollama",
		Model:    "qwen",
	}

	targets, err := ResolveEngineRef(ref, nil, providers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if targets[0].Engine.TimeoutSeconds != 30 {
		t.Errorf("expected timeout 30 from provider, got %d", targets[0].Engine.TimeoutSeconds)
	}
}

func TestResolveEngineRef_ExplicitTimeoutOverrides(t *testing.T) {
	providers := map[string]*Provider{
		"ollama": {Name: "ollama", URL: "http://localhost:11434/v1/chat/completions", TimeoutSeconds: 30},
	}

	ref := EngineRef{
		Provider:       "ollama",
		Model:          "qwen",
		TimeoutSeconds: 60,
	}

	targets, err := ResolveEngineRef(ref, nil, providers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if targets[0].Engine.TimeoutSeconds != 60 {
		t.Errorf("expected explicit timeout 60, got %d", targets[0].Engine.TimeoutSeconds)
	}
}
