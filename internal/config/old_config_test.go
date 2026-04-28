package config

import (
	"encoding/json"
	"testing"
)

func TestConfigLoadLikeOldConfig(t *testing.T) {
	jsonConfig := `{
  "server": { "log_level": "info" },
  "governance": { "tfidf_query_source": "prior_messages" },
  "security_filter": { "entropy_enabled": true, "engine": "security" },
  "window": { "enabled": true, "engine": "window" },
  "compaction": { "prune_stale_tools": true },
  "discovery": { "enabled": true },
  "agents": {
    "security": {
      "models": [
        { "provider": "groq", "model": "llama-3.1-8b-instant" },
        { "provider": "groq", "model": "meta-llama/llama-prompt-guard-2-22m" },
        { "provider": "openrouter", "model": "google/gemma-3-12b-it:free" },
        { "provider": "ollama", "model": "gemma4:e4b" }
      ]
    },
    "window": {
      "system_prompt_file": "/etc/nenya/window.prompt.md",
      "models": [
        "gemini-3.1-flash-lite-preview",
        "gemini-2.5-flash-lite",
        { "provider": "groq", "model": "llama-3.1-8b-instant" },
        { "provider": "nvidia", "model": "google/gemma-2-2b-it" },
        { "provider": "openrouter", "model": "z-ai/glm-4.5-air:free" },
        { "provider": "ollama", "model": "gemma4:e4b" }
      ]
    }
  },
  "providers": {
    "ollama": { "url": "http://127.0.0.1:11434/v1/chat/completions", "timeout_seconds": 240, "auth_style": "bearer" },
    "groq": { "url": "https://api.groq.com/openai/v1/chat/completions", "auth_style": "bearer" },
    "openrouter": { "url": "https://openrouter.ai/api/v1/chat/completions", "auth_style": "bearer" },
    "nvidia": { "url": "https://integrate.api.nvidia.com/v1/chat/completions", "auth_style": "bearer" }
  }
}`

	var cfg Config
	if err := json.Unmarshal([]byte(jsonConfig), &cfg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatalf("apply defaults error: %v", err)
	}

	if len(cfg.SecurityFilter.Engine.ResolvedTargets) == 0 {
		t.Fatal("expected security_filter engine targets to be resolved")
	}
	if cfg.SecurityFilter.Engine.ResolvedTargets[0].Engine.Provider == "" {
		t.Fatal("expected security engine target to have a resolved provider")
	}

	if len(cfg.Window.Engine.ResolvedTargets) == 0 {
		t.Fatal("expected window engine targets to be resolved")
	}
	if cfg.Window.Engine.ResolvedTargets[0].Engine.Provider == "" {
		t.Fatal("expected window engine target to have a resolved provider")
	}
}

func TestConfigLoadWithNoEngines(t *testing.T) {
	jsonConfig := `{
  "security_filter": { "entropy_enabled": true },
  "window": { "enabled": false },
  "governance": { "tfidf_query_source": "prior_messages" },
  "agents": {
    "coder": {
      "models": [{ "provider": "ollama", "model": "qwen2.5-coder" }]
    }
  },
  "providers": {
    "ollama": { "url": "http://localhost:11434/v1/chat/completions", "auth_style": "bearer" }
  }
}`

	var cfg Config
	if err := json.Unmarshal([]byte(jsonConfig), &cfg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatalf("apply defaults error: %v", err)
	}
}

func TestResolveEngineRefs_AgentRefs(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"filter": {
				Models: []AgentModel{
					{Provider: "groq", Model: "llama-3.1-8b-instant"},
				},
			},
		},
		Providers: map[string]ProviderConfig{
			"groq": {URL: "https://api.groq.com/openai/v1/chat/completions"},
		},
	}
	cfg.SecurityFilter.Engine = EngineRef{AgentName: "filter"}

	if err := resolveEngineRefs(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.SecurityFilter.Engine.ResolvedTargets) != 1 {
		t.Fatalf("expected 1 resolved target, got %d", len(cfg.SecurityFilter.Engine.ResolvedTargets))
	}
	if cfg.SecurityFilter.Engine.ResolvedTargets[0].Engine.Provider != "groq" {
		t.Errorf("expected provider groq, got %s", cfg.SecurityFilter.Engine.ResolvedTargets[0].Engine.Provider)
	}
}

func TestResolveEngineRefs_WindowAndSecurityRefs(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"filter": {
				Models: []AgentModel{
					{Provider: "groq", Model: "llama-3.1-8b-instant"},
				},
			},
			"summarizer": {
				Models: []AgentModel{
					{Model: "gemini-2.5-flash-lite"},
				},
			},
		},
		Providers: map[string]ProviderConfig{
			"groq":  {URL: "https://api.groq.com/openai/v1/chat/completions"},
			"gemini": {URL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"},
		},
	}
	cfg.SecurityFilter.Engine = EngineRef{AgentName: "filter"}
	cfg.Window.Engine = EngineRef{AgentName: "summarizer"}

	if err := resolveEngineRefs(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.SecurityFilter.Engine.ResolvedTargets) != 1 {
		t.Fatalf("expected 1 security target, got %d", len(cfg.SecurityFilter.Engine.ResolvedTargets))
	}
	if cfg.SecurityFilter.Engine.ResolvedTargets[0].Engine.Provider != "groq" {
		t.Errorf("expected security provider groq, got %s", cfg.SecurityFilter.Engine.ResolvedTargets[0].Engine.Provider)
	}
	if len(cfg.Window.Engine.ResolvedTargets) != 1 {
		t.Fatalf("expected 1 window target, got %d", len(cfg.Window.Engine.ResolvedTargets))
	}
	if cfg.Window.Engine.ResolvedTargets[0].Engine.Provider != "gemini" {
		t.Errorf("expected window provider gemini (from ModelRegistry), got %s", cfg.Window.Engine.ResolvedTargets[0].Engine.Provider)
	}
}

func TestResolveEngineRefs_AgentWithRegexOnlyModel_Errors(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"regex-agent": {
				Models: []AgentModel{
					{ModelRgx: ".*gemma.*"},
				},
			},
		},
		Providers: map[string]ProviderConfig{
			"groq": {URL: "https://api.groq.com/openai/v1/chat/completions"},
		},
	}
	cfg.SecurityFilter.Engine = EngineRef{AgentName: "regex-agent"}

	if err := resolveEngineRefs(cfg); err == nil {
		t.Fatal("expected error for agent with regex-only model in engine, got nil")
	}
}

func TestResolveEngineRefs_AgentWithURLOverride(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"custom": {
				Models: []AgentModel{
					{
						Provider: "ollama",
						Model:    "qwen2.5-coder",
						URL:      "http://custom:11434/v1/chat/completions",
					},
				},
			},
		},
		Providers: map[string]ProviderConfig{
			"ollama": {URL: "http://localhost:11434/v1/chat/completions"},
		},
	}
	cfg.SecurityFilter.Engine = EngineRef{AgentName: "custom"}

	if err := resolveEngineRefs(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.SecurityFilter.Engine.ResolvedTargets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(cfg.SecurityFilter.Engine.ResolvedTargets))
	}
	if cfg.SecurityFilter.Engine.ResolvedTargets[0].Engine.Provider != "ollama" {
		t.Errorf("expected provider ollama, got %s", cfg.SecurityFilter.Engine.ResolvedTargets[0].Engine.Provider)
	}
	if cfg.SecurityFilter.Engine.ResolvedTargets[0].Provider.URL != "http://custom:11434/v1/chat/completions" {
		t.Errorf("expected URL override, got %s", cfg.SecurityFilter.Engine.ResolvedTargets[0].Provider.URL)
	}
}

func TestResolveEngineRefs_InlineEngineDefaults(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"ollama": {URL: "http://localhost:11434/v1/chat/completions"},
		},
	}

	if err := resolveEngineRefs(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
