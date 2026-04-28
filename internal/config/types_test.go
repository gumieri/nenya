package config

import (
	"encoding/json"
	"testing"
)

func TestAgentConfig_UnmarshalJSON_StringShorthand_DeferredProvider(t *testing.T) {
	jsonData := `{
		"strategy": "fallback",
		"models": ["deepseek-v4-flash", "glm-5-turbo"]
	}`

	var cfg AgentConfig
	err := json.Unmarshal([]byte(jsonData), &cfg)
	if err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if len(cfg.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(cfg.Models))
	}

	if cfg.Models[0].Model != "deepseek-v4-flash" {
		t.Errorf("expected deepseek-v4-flash, got %s", cfg.Models[0].Model)
	}
	if cfg.Models[0].Provider != "" {
		t.Errorf("expected empty provider (deferred), got %s", cfg.Models[0].Provider)
	}
	if cfg.Models[0].MaxContext != 1000000 {
		t.Errorf("expected MaxContext 1000000 from ModelRegistry, got %d", cfg.Models[0].MaxContext)
	}
	if cfg.Models[0].MaxOutput != 393216 {
		t.Errorf("expected MaxOutput 393216 from ModelRegistry, got %d", cfg.Models[0].MaxOutput)
	}

	if cfg.Models[1].Model != "glm-5-turbo" {
		t.Errorf("expected glm-5-turbo, got %s", cfg.Models[1].Model)
	}
	if cfg.Models[1].Provider != "" {
		t.Errorf("expected empty provider (deferred), got %s", cfg.Models[1].Provider)
	}
	if cfg.Models[1].MaxContext != 200000 {
		t.Errorf("expected MaxContext 200000 from ModelRegistry, got %d", cfg.Models[1].MaxContext)
	}
	if cfg.Models[1].MaxOutput != 128000 {
		t.Errorf("expected MaxOutput 128000 from ModelRegistry, got %d", cfg.Models[1].MaxOutput)
	}
}

func TestAgentConfig_UnmarshalJSON_StringShorthand_UnknownModel(t *testing.T) {
	jsonData := `{
		"strategy": "fallback",
		"models": ["totally-unknown-model"]
	}`

	var cfg AgentConfig
	err := json.Unmarshal([]byte(jsonData), &cfg)
	if err == nil {
		t.Fatal("expected error for unknown model, got nil")
	}
}

func TestAgentConfig_UnmarshalJSON_ObjectNotation_Unchanged(t *testing.T) {
	jsonData := `{
		"strategy": "fallback",
		"models": [
			{"provider": "deepseek", "model": "deepseek-v4-flash"},
			{"provider": "zai", "model": "glm-5-turbo", "max_context": 1234}
		]
	}`

	var cfg AgentConfig
	err := json.Unmarshal([]byte(jsonData), &cfg)
	if err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if len(cfg.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(cfg.Models))
	}

	if cfg.Models[0].Provider != "deepseek" {
		t.Errorf("expected deepseek, got %s", cfg.Models[0].Provider)
	}
	if cfg.Models[0].Model != "deepseek-v4-flash" {
		t.Errorf("expected deepseek-v4-flash, got %s", cfg.Models[0].Model)
	}

	if cfg.Models[1].Provider != "zai" {
		t.Errorf("expected zai, got %s", cfg.Models[1].Provider)
	}
	if cfg.Models[1].Model != "glm-5-turbo" {
		t.Errorf("expected glm-5-turbo, got %s", cfg.Models[1].Model)
	}
	if cfg.Models[1].MaxContext != 1234 {
		t.Errorf("expected MaxContext 1234, got %d", cfg.Models[1].MaxContext)
	}
}
