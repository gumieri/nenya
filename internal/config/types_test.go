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
	if cfg.Models[0].MaxOutput != 384000 {
		t.Errorf("expected MaxOutput 384000 from ModelRegistry, got %d", cfg.Models[0].MaxOutput)
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

func TestAgentModel_MatchesCatalog_ExactFields(t *testing.T) {
	tests := []struct {
		name     string
		model    AgentModel
		provider string
		modelID  string
		want     bool
	}{
		{
			name:     "exact provider and model match",
			model:    AgentModel{Provider: "deepseek", Model: "deepseek-v4-flash"},
			provider: "deepseek",
			modelID:  "deepseek-v4-flash",
			want:     true,
		},
		{
			name:     "exact provider matches, model differs",
			model:    AgentModel{Provider: "deepseek", Model: "deepseek-v4-flash"},
			provider: "deepseek",
			modelID:  "deepseek-v4-pro",
			want:     false,
		},
		{
			name:     "exact model matches, provider differs",
			model:    AgentModel{Provider: "deepseek", Model: "deepseek-v4-flash"},
			provider: "nvidia",
			modelID:  "deepseek-v4-flash",
			want:     false,
		},
		{
			name:     "only exact provider set",
			model:    AgentModel{Provider: "deepseek"},
			provider: "deepseek",
			modelID:  "any-model",
			want:     true,
		},
		{
			name:     "only exact model set",
			model:    AgentModel{Model: "deepseek-v4-flash"},
			provider: "any-provider",
			modelID:  "deepseek-v4-flash",
			want:     true,
		},
		{
			name:     "both exact fields empty",
			model:    AgentModel{},
			provider: "deepseek",
			modelID:  "deepseek-v4-flash",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.model.CompileRegex(); err != nil {
				t.Fatalf("failed to compile regex: %v", err)
			}
			got := tt.model.MatchesCatalog(tt.provider, tt.modelID)
			if got != tt.want {
				t.Errorf("MatchesCatalog() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAgentModel_MatchesCatalog_Regex(t *testing.T) {
	tests := []struct {
		name     string
		model    AgentModel
		provider string
		modelID  string
		want     bool
	}{
		{
			name:     "model regex matches",
			model:    AgentModel{ModelRgx: "^deepseek-v4-.*$"},
			provider: "deepseek",
			modelID:  "deepseek-v4-flash",
			want:     true,
		},
		{
			name:     "model regex does not match",
			model:    AgentModel{ModelRgx: "^deepseek-v4-.*$"},
			provider: "deepseek",
			modelID:  "gemini-2.5-flash",
			want:     false,
		},
		{
			name:     "provider regex matches",
			model:    AgentModel{ProviderRgx: "^deep"},
			provider: "deepseek",
			modelID:  "deepseek-v4-flash",
			want:     true,
		},
		{
			name:     "provider regex does not match",
			model:    AgentModel{ProviderRgx: "^gem"},
			provider: "deepseek",
			modelID:  "deepseek-v4-flash",
			want:     false,
		},
		{
			name:     "both regex patterns match",
			model:    AgentModel{ProviderRgx: "^deep", ModelRgx: ".*v4-flash$"},
			provider: "deepseek",
			modelID:  "deepseek-v4-flash",
			want:     true,
		},
		{
			name:     "both regex patterns, one fails",
			model:    AgentModel{ProviderRgx: "^deep", ModelRgx: ".*v4-pro$"},
			provider: "deepseek",
			modelID:  "deepseek-v4-flash",
			want:     false,
		},
		{
			name:     "exact provider + model regex",
			model:    AgentModel{Provider: "deepseek", ModelRgx: ".*v4-.*$"},
			provider: "deepseek",
			modelID:  "deepseek-v4-flash",
			want:     true,
		},
		{
			name:     "exact provider + model regex, provider mismatch",
			model:    AgentModel{Provider: "gemini", ModelRgx: "^v4-.*$"},
			provider: "deepseek",
			modelID:  "deepseek-v4-flash",
			want:     false,
		},
		{
			name:     "provider regex + exact model",
			model:    AgentModel{ProviderRgx: "^deep", Model: "deepseek-v4-flash"},
			provider: "deepseek",
			modelID:  "deepseek-v4-flash",
			want:     true,
		},
		{
			name:     "provider regex + exact model, model mismatch",
			model:    AgentModel{ProviderRgx: "^deep", Model: "deepseek-v4-pro"},
			provider: "deepseek",
			modelID:  "deepseek-v4-flash",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.model.CompileRegex(); err != nil {
				t.Fatalf("failed to compile regex: %v", err)
			}
			got := tt.model.MatchesCatalog(tt.provider, tt.modelID)
			if got != tt.want {
				t.Errorf("MatchesCatalog() = %v, want %v", got, tt.want)
			}
		})
	}
}
