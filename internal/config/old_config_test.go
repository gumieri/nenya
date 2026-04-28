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
