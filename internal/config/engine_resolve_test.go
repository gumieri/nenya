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

func TestBuildAgentEngineTarget_StringShorthandResolvesProvider(t *testing.T) {
	providers := map[string]*Provider{
		"gemini": {
			Name: "gemini",
			URL:  "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
		},
	}

	ref := EngineRef{
		AgentName: "window",
	}
	agent := AgentConfig{
		Models: []AgentModel{
			{Model: "gemini-3.1-flash-lite-preview"},
		},
	}

	targets, err := resolveAgentEngineRef(ref, map[string]AgentConfig{"window": agent}, providers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].Engine.Provider != "gemini" {
		t.Errorf("expected resolved provider gemini, got %s", targets[0].Engine.Provider)
	}
	if targets[0].Engine.Model != "gemini-3.1-flash-lite-preview" {
		t.Errorf("expected model gemini-3.1-flash-lite-preview, got %s", targets[0].Engine.Model)
	}
	if targets[0].Provider.URL == "" {
		t.Fatal("expected resolved provider URL")
	}
}

func TestBuildAgentEngineTarget_StringShorthandMixedOrder(t *testing.T) {
	providers := map[string]*Provider{
		"gemini": {Name: "gemini", URL: "https://generativelanguage.googleapis.com/v1/chat/completions"},
		"groq":   {Name: "groq", URL: "https://api.groq.com/openai/v1/chat/completions"},
	}

	ref := EngineRef{
		AgentName: "window",
	}
	agent := AgentConfig{
		Models: []AgentModel{
			{Model: "gemini-3.1-flash-lite-preview"},
			{Provider: "groq", Model: "llama-3.1-8b-instant"},
			{Model: "gemini-2.5-flash-lite"},
		},
	}

	targets, err := resolveAgentEngineRef(ref, map[string]AgentConfig{"window": agent}, providers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 3 {
		t.Fatalf("expected 3 targets, got %d", len(targets))
	}
	if targets[0].Engine.Provider != "gemini" {
		t.Errorf("expected target[0] provider gemini, got %s", targets[0].Engine.Provider)
	}
	if targets[1].Engine.Provider != "groq" {
		t.Errorf("expected target[1] provider groq, got %s", targets[1].Engine.Provider)
	}
	if targets[2].Engine.Provider != "gemini" {
		t.Errorf("expected target[2] provider gemini, got %s", targets[2].Engine.Provider)
	}
}

func TestBuildAgentEngineTarget_StringShorthandUnknownModel(t *testing.T) {
	providers := map[string]*Provider{
		"ollama": {Name: "ollama", URL: "http://localhost:11434/v1/chat/completions"},
	}

	ref := EngineRef{
		AgentName: "window",
	}
	agent := AgentConfig{
		Models: []AgentModel{
			{Model: "nonexistent-model-that-is-not-in-registry"},
		},
	}

	_, err := resolveAgentEngineRef(ref, map[string]AgentConfig{"window": agent}, providers)
	if err == nil {
		t.Fatal("expected error for unknown model with no provider")
	}
}

func TestResolveSingleEngineRef_EmptyRefSkips(t *testing.T) {
	ref := &EngineRef{}
	err := resolveSingleEngineRef(ref, nil, nil, "test")
	if err != nil {
		t.Fatalf("expected nil for empty EngineRef, got: %v", err)
	}
	if ref.ResolvedTargets != nil {
		t.Errorf("expected ResolvedTargets to be nil for empty EngineRef, got %v", ref.ResolvedTargets)
	}
}

func TestResolveEngineRefs_NoEnginesConfigured(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"ollama": {URL: "http://localhost:11434/v1/chat/completions"},
		},
	}

	err := resolveEngineRefs(cfg)
	if err != nil {
		t.Fatalf("expected no error when no engines configured, got: %v", err)
	}
}

func TestResolveAgentEngineRef_SkipsFailingFirstModel(t *testing.T) {
	providers := map[string]*Provider{
		"groq": {Name: "groq", URL: "https://api.groq.com/openai/v1/chat/completions"},
	}
	agent := AgentConfig{
		Models: []AgentModel{
			{Model: "nonexistent-model-not-in-registry"},
			{Provider: "groq", Model: "llama-3.1-8b-instant"},
		},
	}
	ref := EngineRef{AgentName: "test-agent"}

	targets, err := resolveAgentEngineRef(ref, map[string]AgentConfig{"test-agent": agent}, providers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 target (from second model), got %d", len(targets))
	}
	if targets[0].Engine.Provider != "groq" {
		t.Errorf("expected provider groq, got %s", targets[0].Engine.Provider)
	}
	if targets[0].Engine.Model != "llama-3.1-8b-instant" {
		t.Errorf("expected model llama-3.1-8b-instant, got %s", targets[0].Engine.Model)
	}
}

func TestResolveAgentEngineRef_SkipsMultipleFailingModels(t *testing.T) {
	providers := map[string]*Provider{
		"ollama": {Name: "ollama", URL: "http://localhost:11434/v1/chat/completions"},
	}
	agent := AgentConfig{
		Models: []AgentModel{
			{Model: "unknown-model-a"},
			{ModelRgx: ".*regex.*"},
			{Provider: "unknown-provider", Model: "some-model"},
			{Provider: "ollama", Model: "qwen2.5-coder:7b"},
		},
	}
	ref := EngineRef{AgentName: "test-agent"}

	targets, err := resolveAgentEngineRef(ref, map[string]AgentConfig{"test-agent": agent}, providers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 target (from fourth model), got %d", len(targets))
	}
	if targets[0].Engine.Provider != "ollama" {
		t.Errorf("expected provider ollama, got %s", targets[0].Engine.Provider)
	}
}

func TestResolveAgentEngineRef_AllModelsFail(t *testing.T) {
	providers := map[string]*Provider{
		"ollama": {Name: "ollama", URL: "http://localhost:11434/v1/chat/completions"},
	}
	agent := AgentConfig{
		Models: []AgentModel{
			{Model: "unknown-model-a"},
			{ModelRgx: ".*regex.*"},
		},
	}
	ref := EngineRef{AgentName: "test-agent"}

	_, err := resolveAgentEngineRef(ref, map[string]AgentConfig{"test-agent": agent}, providers)
	if err == nil {
		t.Fatal("expected error when all models fail to resolve")
	}
}

func TestResolveAgentEngineRef_SkipsRegexOnlyModels(t *testing.T) {
	providers := map[string]*Provider{
		"groq": {Name: "groq", URL: "https://api.groq.com/openai/v1/chat/completions"},
	}
	agent := AgentConfig{
		Models: []AgentModel{
			{ModelRgx: ".*gemma.*"},
			{Provider: "groq", Model: "llama-3.1-8b-instant"},
			{ModelRgx: ".*guard.*"},
		},
	}
	ref := EngineRef{AgentName: "test-agent"}

	targets, err := resolveAgentEngineRef(ref, map[string]AgentConfig{"test-agent": agent}, providers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 target (from second model), got %d", len(targets))
	}
	if targets[0].Engine.Provider != "groq" {
		t.Errorf("expected provider groq, got %s", targets[0].Engine.Provider)
	}
}

func TestResolveAgentEngineRef_ModelOnlyNotInRegistry_Skips(t *testing.T) {
	providers := map[string]*Provider{
		"groq": {Name: "groq", URL: "https://api.groq.com/openai/v1/chat/completions"},
	}
	agent := AgentConfig{
		Models: []AgentModel{
			{Model: "llama-3.1-8b-instant"},
			{Provider: "groq", Model: "llama-3.1-70b-versatile"},
		},
	}
	ref := EngineRef{AgentName: "test-agent"}

	targets, err := resolveAgentEngineRef(ref, map[string]AgentConfig{"test-agent": agent}, providers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 target (from second model), got %d", len(targets))
	}
	if targets[0].Engine.Model != "llama-3.1-70b-versatile" {
		t.Errorf("expected model llama-3.1-70b-versatile, got %s", targets[0].Engine.Model)
	}
}
