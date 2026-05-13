package config

import (
	"testing"
)

func TestPricingOverride_IsZero(t *testing.T) {
	tests := []struct {
		name  string
		p     PricingOverride
		zero  bool
	}{
		{
			name: "both zero",
			p:    PricingOverride{},
			zero: true,
		},
		{
			name: "input cost set",
			p:    PricingOverride{InputCostPer1M: 1.0},
			zero: false,
		},
		{
			name: "output cost set",
			p:    PricingOverride{OutputCostPer1M: 1.0},
			zero: false,
		},
		{
			name: "both set",
			p:    PricingOverride{InputCostPer1M: 1.0, OutputCostPer1M: 0.5},
			zero: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.p.IsZero() != tt.zero {
				t.Errorf("IsZero() = %v, want %v", tt.p.IsZero(), tt.zero)
			}
		})
	}
}

func TestPricingOverride_Validate(t *testing.T) {
	tests := []struct {
		name    string
		p       PricingOverride
		wantErr bool
	}{
		{
			name:    "valid zero",
			p:       PricingOverride{},
			wantErr: false,
		},
		{
			name:    "valid positive",
			p:       PricingOverride{InputCostPer1M: 1.0, OutputCostPer1M: 0.5},
			wantErr: false,
		},
		{
			name:    "negative input",
			p:       PricingOverride{InputCostPer1M: -1.0},
			wantErr: true,
		},
		{
			name:    "negative output",
			p:       PricingOverride{OutputCostPer1M: -1.0},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.p.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestModelEntry_Validate(t *testing.T) {
	tests := []struct {
		name    string
		e       ModelEntry
		wantErr bool
	}{
		{
			name:    "valid minimal",
			e:       ModelEntry{Provider: "openai"},
			wantErr: false,
		},
		{
			name:    "valid with limits",
			e:       ModelEntry{Provider: "openai", MaxContext: 128000, MaxOutput: 4096},
			wantErr: false,
		},
		{
			name:    "valid with pricing",
			e:       ModelEntry{Provider: "openai", Pricing: PricingOverride{InputCostPer1M: 1.0}},
			wantErr: false,
		},
		{
			name:    "empty provider",
			e:       ModelEntry{},
			wantErr: true,
		},
		{
			name:    "negative max context",
			e:       ModelEntry{Provider: "openai", MaxContext: -1},
			wantErr: true,
		},
		{
			name:    "negative max output",
			e:       ModelEntry{Provider: "openai", MaxOutput: -1},
			wantErr: true,
		},
		{
			name:    "invalid pricing",
			e:       ModelEntry{Provider: "openai", Pricing: PricingOverride{InputCostPer1M: -1.0}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.e.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestModelThinkingConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		c       ModelThinkingConfig
		wantErr bool
	}{
		{
			name:    "valid zero",
			c:       ModelThinkingConfig{},
			wantErr: false,
		},
		{
			name:    "valid min only",
			c:       ModelThinkingConfig{Min: 1024},
			wantErr: false,
		},
		{
			name:    "valid max only",
			c:       ModelThinkingConfig{Max: 2048},
			wantErr: false,
		},
		{
			name:    "valid min less than max",
			c:       ModelThinkingConfig{Min: 1024, Max: 2048},
			wantErr: false,
		},
		{
			name:    "valid min equals max",
			c:       ModelThinkingConfig{Min: 1024, Max: 1024},
			wantErr: false,
		},
		{
			name:    "valid with zero allowed",
			c:       ModelThinkingConfig{Min: 1024, Max: 2048, ZeroAllowed: true},
			wantErr: false,
		},
		{
			name:    "valid with levels",
			c:       ModelThinkingConfig{Min: 1024, Max: 2048, Levels: []string{"low", "high"}},
			wantErr: false,
		},
		{
			name:    "valid dynamic allowed",
			c:       ModelThinkingConfig{Min: 1024, Max: 2048, DynamicAllowed: true},
			wantErr: false,
		},
		{
			name:    "invalid negative min",
			c:       ModelThinkingConfig{Min: -1},
			wantErr: true,
		},
		{
			name:    "invalid negative max",
			c:       ModelThinkingConfig{Max: -1},
			wantErr: true,
		},
		{
			name:    "invalid negative both",
			c:       ModelThinkingConfig{Min: -1, Max: -1},
			wantErr: true,
		},
		{
			name:    "invalid min greater than max",
			c:       ModelThinkingConfig{Min: 2048, Max: 1024},
			wantErr: true,
		},
		{
			name:    "invalid min greater than max with zero",
			c:       ModelThinkingConfig{Min: 2048, Max: 1024, ZeroAllowed: true},
			wantErr: true,
		},
		{
			name:    "invalid min greater than max with levels",
			c:       ModelThinkingConfig{Min: 2048, Max: 1024, Levels: []string{"low"}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.c.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestAgentModel_CompileRegex_Valid(t *testing.T) {
	tests := []struct {
		name    string
		m       AgentModel
		wantErr bool
	}{
		{
			name: "no regex",
			m: AgentModel{
				Provider: "openai",
				Model:    "gpt-4",
			},
			wantErr: false,
		},
		{
			name: "valid provider regex",
			m: AgentModel{
				ProviderRgx: "openai|anthropic",
			},
			wantErr: false,
		},
		{
			name: "valid model regex",
			m: AgentModel{
				ModelRgx: "gpt-\\d+|claude-\\d+",
			},
			wantErr: false,
		},
		{
			name: "invalid provider regex",
			m: AgentModel{
				ProviderRgx: "[invalid",
			},
			wantErr: true,
		},
		{
			name: "invalid model regex",
			m: AgentModel{
				ModelRgx: "(",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.m.CompileRegex()
			if (err != nil) != tt.wantErr {
				t.Errorf("CompileRegex() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestAgentModel_CompileRegex_CompilesFields(t *testing.T) {
	m := AgentModel{
		ProviderRgx: "openai|anthropic",
		ModelRgx:    "gpt-\\d+|claude-\\d+",
	}

	err := m.CompileRegex()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if m.providerRE == nil {
		t.Error("expected providerRE to be compiled")
	}
	if m.modelRE == nil {
		t.Error("expected modelRE to be compiled")
	}
}

func TestAgentModel_MatchesCatalog(t *testing.T) {
	tests := []struct {
		name        string
		m           AgentModel
		provider    string
		model       string
		wantMatch   bool
		setupCompile bool
	}{
		{
			name: "exact match both",
			m: AgentModel{
				Provider: "openai",
				Model:    "gpt-4",
			},
			provider:  "openai",
			model:    "gpt-4",
			wantMatch: true,
		},
		{
			name: "exact provider, different model",
			m: AgentModel{
				Provider: "openai",
				Model:    "gpt-4",
			},
			provider:  "openai",
			model:    "gpt-3.5",
			wantMatch: false,
		},
		{
			name: "provider regex match",
			m: AgentModel{
				ProviderRgx: "openai|anthropic",
			},
			provider:      "openai",
			model:         "gpt-4",
			wantMatch:     true,
			setupCompile:  true,
		},
		{
			name: "model regex match",
			m: AgentModel{
				ModelRgx: "gpt-\\d+",
			},
			provider:     "openai",
			model:        "gpt-4",
			wantMatch:    true,
			setupCompile: true,
		},
		{
			name: "both regex match",
			m: AgentModel{
				ProviderRgx: "openai|anthropic",
				ModelRgx:    "gpt-\\d+|claude-\\d+",
			},
			provider:     "anthropic",
			model:        "claude-3",
			wantMatch:    true,
			setupCompile: true,
		},
		{
			name: "provider regex no match",
			m: AgentModel{
				ProviderRgx: "openai",
			},
			provider:     "anthropic",
			model:        "claude-3",
			wantMatch:    false,
			setupCompile: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setupCompile {
				if err := tt.m.CompileRegex(); err != nil {
					t.Fatalf("failed to compile regex: %v", err)
				}
			}
			if got := tt.m.MatchesCatalog(tt.provider, tt.model); got != tt.wantMatch {
				t.Errorf("MatchesCatalog() = %v, want %v", got, tt.wantMatch)
			}
		})
	}
}

func TestAgentModel_IsDynamic(t *testing.T) {
	tests := []struct {
		name         string
		m            AgentModel
		wantDynamic  bool
		setupCompile bool
	}{
		{
			name:        "static",
			m:           AgentModel{Provider: "openai", Model: "gpt-4"},
			wantDynamic: false,
		},
		{
			name: "provider regex only",
			m: AgentModel{
				ProviderRgx: "openai|anthropic",
			},
			wantDynamic:  true,
			setupCompile: true,
		},
		{
			name: "model regex only",
			m: AgentModel{
				ModelRgx: "gpt-\\d+",
			},
			wantDynamic:  true,
			setupCompile: true,
		},
		{
			name: "both regex",
			m: AgentModel{
				ProviderRgx: "openai",
				ModelRgx:    "gpt-\\d+",
			},
			wantDynamic:  true,
			setupCompile: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setupCompile {
				if err := tt.m.CompileRegex(); err != nil {
					t.Fatalf("failed to compile regex: %v", err)
				}
			}
			if got := tt.m.IsDynamic(); got != tt.wantDynamic {
				t.Errorf("IsDynamic() = %v, want %v", got, tt.wantDynamic)
			}
		})
	}
}

func TestProviderEntry_ToProviderConfig(t *testing.T) {
	e := ProviderEntry{
		URL:        "https://api.openai.com/v1/chat/completions",
		AuthStyle:  "bearer",
		ApiFormat:  "openai",
		FormatURLs: map[string]string{
			"anthropic": "https://api.openai.com/v1/anthropic/messages",
		},
		Models: []ModelRef{
			{ID: "gpt-4", MaxContext: 128000, MaxOutput: 4096},
		},
	}

	cfg := e.ToProviderConfig()
	if cfg.URL != e.URL {
		t.Errorf("URL = %v, want %v", cfg.URL, e.URL)
	}
	if cfg.AuthStyle != e.AuthStyle {
		t.Errorf("AuthStyle = %v, want %v", cfg.AuthStyle, e.AuthStyle)
	}
	if cfg.ApiFormat != e.ApiFormat {
		t.Errorf("ApiFormat = %v, want %v", cfg.ApiFormat, e.ApiFormat)
	}
	if cfg.FormatURLs["anthropic"] != e.FormatURLs["anthropic"] {
		t.Errorf("FormatURLs mismatch")
	}
}

func TestAgentConfig_UnmarshalJSON_StringShorthand(t *testing.T) {
	data := `{
		"models": [
			"gpt-4o",
			"claude-sonnet-4-5"
		]
	}`

	var cfg AgentConfig
	err := cfg.UnmarshalJSON([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cfg.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(cfg.Models))
	}

	if cfg.Models[0].Model != "gpt-4o" {
		t.Errorf("expected 'gpt-4o', got %s", cfg.Models[0].Model)
	}
	if cfg.Models[1].Model != "claude-sonnet-4-5" {
		t.Errorf("expected 'claude-sonnet-4-5', got %s", cfg.Models[1].Model)
	}
}

func TestAgentConfig_UnmarshalJSON_ObjectForm(t *testing.T) {
	data := `{
		"models": [
			{
				"provider": "openai",
				"model": "gpt-4",
				"max_context": 128000,
				"max_output": 4096
			}
		]
	}`

	var cfg AgentConfig
	err := cfg.UnmarshalJSON([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cfg.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(cfg.Models))
	}

	m := cfg.Models[0]
	if m.Provider != "openai" {
		t.Errorf("expected 'openai', got %s", m.Provider)
	}
	if m.Model != "gpt-4" {
		t.Errorf("expected 'gpt-4', got %s", m.Model)
	}
	if m.MaxContext != 128000 {
		t.Errorf("expected 128000, got %d", m.MaxContext)
	}
}

func TestAgentConfig_UnmarshalJSON_UnknownModel(t *testing.T) {
	data := `{
		"models": ["unknown-model"]
	}`

	var cfg AgentConfig
	err := cfg.UnmarshalJSON([]byte(data))
	if err == nil {
		t.Error("expected error for unknown model")
	}
}

func TestAgentConfig_UnmarshalJSON_MixedForms(t *testing.T) {
	data := `{
		"models": [
			"gpt-4o",
			{
				"provider": "anthropic",
				"model": "claude-3-opus",
				"max_context": 200000
			}
		]
	}`

	var cfg AgentConfig
	err := cfg.UnmarshalJSON([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cfg.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(cfg.Models))
	}

	if cfg.Models[0].Model != "gpt-4o" {
		t.Errorf("expected 'gpt-4o', got %s", cfg.Models[0].Model)
	}

	if cfg.Models[1].Provider != "anthropic" {
		t.Errorf("expected 'anthropic', got %s", cfg.Models[1].Provider)
	}
}
