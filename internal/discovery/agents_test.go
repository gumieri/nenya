package discovery

import (
	"log/slog"
	"testing"

	"nenya/internal/config"
)

func TestAutoAgentsConfig_IsEnabled(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *config.AutoAgentsConfig
		category string
		want     bool
	}{
		{
			name:     "nil config enables all",
			cfg:      nil,
			category: "fast",
			want:     true,
		},
		{
			name:     "nil config enables all categories",
			cfg:      nil,
			category: "reasoning",
			want:     true,
		},
		{
			name:     "nil config enables unknown category",
			cfg:      nil,
			category: "unknown",
			want:     true,
		},
		{
			name: "empty config disables all",
			cfg: &config.AutoAgentsConfig{
				Fast:      nil,
				Reasoning: nil,
			},
			category: "fast",
			want:     false,
		},
		{
			name: "explicit enabled",
			cfg: &config.AutoAgentsConfig{
				Fast: &config.AutoAgentCategoryConfig{Enabled: true},
			},
			category: "fast",
			want:     true,
		},
		{
			name: "explicit disabled",
			cfg: &config.AutoAgentsConfig{
				Fast: &config.AutoAgentCategoryConfig{Enabled: false},
			},
			category: "fast",
			want:     false,
		},
		{
			name: "one enabled, one disabled",
			cfg: &config.AutoAgentsConfig{
				Fast:      &config.AutoAgentCategoryConfig{Enabled: true},
				Reasoning: &config.AutoAgentCategoryConfig{Enabled: false},
			},
			category: "fast",
			want:     true,
		},
		{
			name: "one enabled, one disabled - check disabled",
			cfg: &config.AutoAgentsConfig{
				Fast:      &config.AutoAgentCategoryConfig{Enabled: true},
				Reasoning: &config.AutoAgentCategoryConfig{Enabled: false},
			},
			category: "reasoning",
			want:     false,
		},
		{
			name:     "unknown category returns false",
			cfg:      &config.AutoAgentsConfig{},
			category: "unknown",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.IsEnabled(tt.category)
			if got != tt.want {
				t.Errorf("IsEnabled(%q) = %v, want %v", tt.category, got, tt.want)
			}
		})
	}
}

func TestDiscoveredModel_HasCapability(t *testing.T) {
	tests := []struct {
		name      string
		model     DiscoveredModel
		capability string
		want      bool
	}{
		{
			name:      "nil metadata returns false",
			model:     DiscoveredModel{ID: "test", Metadata: nil},
			capability: "vision",
			want:      false,
		},
		{
			name: "vision capability true",
			model: DiscoveredModel{
				ID: "claude-3-5-sonnet",
				Metadata: &ModelMetadata{
					SupportsVision: true,
				},
			},
			capability: "vision",
			want:      true,
		},
		{
			name: "vision capability false",
			model: DiscoveredModel{
				ID: "gpt-4",
				Metadata: &ModelMetadata{
					SupportsVision: false,
				},
			},
			capability: "vision",
			want:      false,
		},
		{
			name: "tool_calls capability true",
			model: DiscoveredModel{
				ID: "claude-3-5-sonnet",
				Metadata: &ModelMetadata{
					SupportsToolCalls: true,
				},
			},
			capability: "tool_calls",
			want:      true,
		},
		{
			name: "reasoning capability true",
			model: DiscoveredModel{
				ID: "deepseek-v4",
				Metadata: &ModelMetadata{
					SupportsReasoning: true,
				},
			},
			capability: "reasoning",
			want:      true,
		},
		{
			name: "content_arrays capability true",
			model: DiscoveredModel{
				ID: "gpt-4o",
				Metadata: &ModelMetadata{
					SupportsContentArrays: true,
				},
			},
			capability: "content_arrays",
			want:      true,
		},
		{
			name: "stream_options capability true",
			model: DiscoveredModel{
				ID: "gemini-2.5-flash",
				Metadata: &ModelMetadata{
					SupportsStreamOptions: true,
				},
			},
			capability: "stream_options",
			want:      true,
		},
		{
			name: "unknown capability returns false",
			model: DiscoveredModel{
				ID: "test",
				Metadata: &ModelMetadata{},
			},
			capability: "unknown",
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.model.HasCapability(tt.capability)
			if got != tt.want {
				t.Errorf("HasCapability(%q) = %v, want %v", tt.capability, got, tt.want)
			}
		})
	}
}

func TestIsFastModel(t *testing.T) {
	tests := []struct {
		name  string
		model DiscoveredModel
		want  bool
	}{
		{
			name:  "fast model - small context and output",
			model: DiscoveredModel{MaxContext: 32000, MaxOutput: 4096},
			want:  true,
		},
		{
			name:  "fast model - at boundary",
			model: DiscoveredModel{MaxContext: 32000, MaxOutput: 4096},
			want:  true,
		},
		{
			name:  "not fast - context too large",
			model: DiscoveredModel{MaxContext: 33000, MaxOutput: 4096},
			want:  false,
		},
		{
			name:  "not fast - output too large",
			model: DiscoveredModel{MaxContext: 32000, MaxOutput: 5000},
			want:  false,
		},
		{
			name:  "not fast - zero context",
			model: DiscoveredModel{MaxContext: 0, MaxOutput: 4096},
			want:  false,
		},
		{
			name:  "not fast - zero output",
			model: DiscoveredModel{MaxContext: 32000, MaxOutput: 0},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isFastModel(tt.model)
			if got != tt.want {
				t.Errorf("isFastModel() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsReasoningModel(t *testing.T) {
	tests := []struct {
		name  string
		model DiscoveredModel
		want  bool
	}{
		{
			name: "reasoning model - large context with reasoning",
			model: DiscoveredModel{
				MaxContext: 128000,
				Metadata:   &ModelMetadata{SupportsReasoning: true},
			},
			want: true,
		},
		{
			name: "not reasoning - large context without reasoning",
			model: DiscoveredModel{
				MaxContext: 128000,
				Metadata:   &ModelMetadata{SupportsReasoning: false},
			},
			want: false,
		},
		{
			name: "not reasoning - reasoning but small context",
			model: DiscoveredModel{
				MaxContext: 64000,
				Metadata:   &ModelMetadata{SupportsReasoning: true},
			},
			want: false,
		},
		{
			name: "not reasoning - no metadata",
			model: DiscoveredModel{
				MaxContext: 128000,
				Metadata:   nil,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isReasoningModel(tt.model)
			if got != tt.want {
				t.Errorf("isReasoningModel() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsVisionModel(t *testing.T) {
	tests := []struct {
		name  string
		model DiscoveredModel
		want  bool
	}{
		{
			name: "vision model",
			model: DiscoveredModel{
				Metadata: &ModelMetadata{SupportsVision: true},
			},
			want: true,
		},
		{
			name: "not vision",
			model: DiscoveredModel{
				Metadata: &ModelMetadata{SupportsVision: false},
			},
			want: false,
		},
		{
			name:  "not vision - no metadata",
			model: DiscoveredModel{Metadata: nil},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isVisionModel(tt.model)
			if got != tt.want {
				t.Errorf("isVisionModel() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsToolModel(t *testing.T) {
	tests := []struct {
		name  string
		model DiscoveredModel
		want  bool
	}{
		{
			name: "tool model",
			model: DiscoveredModel{
				Metadata: &ModelMetadata{SupportsToolCalls: true},
			},
			want: true,
		},
		{
			name: "not tool",
			model: DiscoveredModel{
				Metadata: &ModelMetadata{SupportsToolCalls: false},
			},
			want: false,
		},
		{
			name:  "not tool - no metadata",
			model: DiscoveredModel{Metadata: nil},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isToolModel(tt.model)
			if got != tt.want {
				t.Errorf("isToolModel() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsLargeModel(t *testing.T) {
	tests := []struct {
		name  string
		model DiscoveredModel
		want  bool
	}{
		{
			name:  "large model",
			model: DiscoveredModel{MaxContext: 200000},
			want:  true,
		},
		{
			name:  "large model - at boundary",
			model: DiscoveredModel{MaxContext: 200000},
			want:  true,
		},
		{
			name:  "not large - below threshold",
			model: DiscoveredModel{MaxContext: 199999},
			want:  false,
		},
		{
			name:  "not large - zero context",
			model: DiscoveredModel{MaxContext: 0},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isLargeModel(tt.model)
			if got != tt.want {
				t.Errorf("isLargeModel() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsBalancedModel(t *testing.T) {
	tests := []struct {
		name  string
		model DiscoveredModel
		want  bool
	}{
		{
			name:  "balanced model - middle range",
			model: DiscoveredModel{MaxContext: 64000},
			want:  true,
		},
		{
			name:  "balanced model - at lower boundary",
			model: DiscoveredModel{MaxContext: 32001},
			want:  true,
		},
		{
			name:  "balanced model - at upper boundary",
			model: DiscoveredModel{MaxContext: 127999},
			want:  true,
		},
		{
			name:  "not balanced - too small",
			model: DiscoveredModel{MaxContext: 32000},
			want:  false,
		},
		{
			name:  "not balanced - too large",
			model: DiscoveredModel{MaxContext: 128000},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isBalancedModel(tt.model)
			if got != tt.want {
				t.Errorf("isBalancedModel() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsCodingModel(t *testing.T) {
	tests := []struct {
		name  string
		model DiscoveredModel
		want  bool
	}{
		{
			name: "coding model - codestral with tools",
			model: DiscoveredModel{
				ID: "codestral-latest",
				Metadata: &ModelMetadata{SupportsToolCalls: true},
			},
			want: true,
		},
		{
			name: "coding model - deepseek-v4 with tools",
			model: DiscoveredModel{
				ID: "deepseek-v4-pro",
				Metadata: &ModelMetadata{SupportsToolCalls: true},
			},
			want: true,
		},
		{
			name: "coding model - qwen2.5 with tools",
			model: DiscoveredModel{
				ID: "qwen2.5-72b-turbo",
				Metadata: &ModelMetadata{SupportsToolCalls: true},
			},
			want: true,
		},
		{
			name: "coding model - claude-sonnet with tools",
			model: DiscoveredModel{
				ID: "claude-sonnet-4-5",
				Metadata: &ModelMetadata{SupportsToolCalls: true},
			},
			want: true,
		},
		{
			name: "not coding - no tools",
			model: DiscoveredModel{
				ID: "gpt-4o",
				Metadata: &ModelMetadata{SupportsToolCalls: false},
			},
			want: false,
		},
		{
			name: "not coding - tools but unknown prefix",
			model: DiscoveredModel{
				ID: "unknown-model",
				Metadata: &ModelMetadata{SupportsToolCalls: true},
			},
			want: false,
		},
		{
			name: "not coding - no metadata",
			model: DiscoveredModel{
				ID: "test-model",
				Metadata: nil,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isCodingModel(tt.model)
			if got != tt.want {
				t.Errorf("isCodingModel() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGenerateAutoAgents(t *testing.T) {
	logger := slog.New(slog.Default().Handler())

	// Create a catalog with some test models
	catalog := NewModelCatalog()
	catalog.Add(DiscoveredModel{
		ID:         "fast-model",
		Provider:   "test",
		MaxContext: 32000,
		MaxOutput:  4096,
		Metadata:   &ModelMetadata{},
	})
	catalog.Add(DiscoveredModel{
		ID:         "reasoning-model",
		Provider:   "test",
		MaxContext: 128000,
		MaxOutput:  8192,
		Metadata:   &ModelMetadata{SupportsReasoning: true},
	})
	catalog.Add(DiscoveredModel{
		ID:         "vision-model",
		Provider:   "test",
		MaxContext: 128000,
		MaxOutput:  8192,
		Metadata:   &ModelMetadata{SupportsVision: true},
	})
	catalog.Add(DiscoveredModel{
		ID:         "tool-model",
		Provider:   "test",
		MaxContext: 128000,
		MaxOutput:  8192,
		Metadata:   &ModelMetadata{SupportsToolCalls: true},
	})
	catalog.Add(DiscoveredModel{
		ID:         "large-model",
		Provider:   "test",
		MaxContext: 200000,
		MaxOutput:  8192,
		Metadata:   &ModelMetadata{},
	})
	catalog.Add(DiscoveredModel{
		ID:         "balanced-model",
		Provider:   "test",
		MaxContext: 64000,
		MaxOutput:  8192,
		Metadata:   &ModelMetadata{},
	})
	catalog.Add(DiscoveredModel{
		ID:         "codestral-latest",
		Provider:   "test",
		MaxContext: 128000,
		MaxOutput:  8192,
		Metadata:   &ModelMetadata{SupportsToolCalls: true},
	})

	// Create providers map with API key
	providers := map[string]*config.Provider{
		"test": {
			Name:     "test",
			APIKey:   "test-key",
			AuthStyle: "bearer",
		},
	}

	// Test with nil config (all enabled)
	t.Run("nil config enables all", func(t *testing.T) {
		agents := GenerateAutoAgents(catalog, providers, nil, logger)

		expectedAgents := []string{
			"auto_fast",
			"auto_reasoning",
			"auto_vision",
			"auto_tools",
			"auto_large",
			"auto_balanced",
			"auto_coding",
		}

		for _, name := range expectedAgents {
			if _, ok := agents[name]; !ok {
				t.Errorf("expected agent %q not generated", name)
			}
		}

		if len(agents) != len(expectedAgents) {
			t.Errorf("expected %d agents, got %d", len(expectedAgents), len(agents))
		}
	})

	// Test with empty config (all disabled)
	t.Run("empty config disables all", func(t *testing.T) {
		cfg := &config.AutoAgentsConfig{}
		agents := GenerateAutoAgents(catalog, providers, cfg, logger)

		if len(agents) != 0 {
			t.Errorf("expected 0 agents with empty config, got %d", len(agents))
		}
	})

	// Test with selective enable
	t.Run("selective enable", func(t *testing.T) {
		cfg := &config.AutoAgentsConfig{
			Fast:      &config.AutoAgentCategoryConfig{Enabled: true},
			Reasoning: &config.AutoAgentCategoryConfig{Enabled: true},
		}
		agents := GenerateAutoAgents(catalog, providers, cfg, logger)

		expectedAgents := []string{"auto_fast", "auto_reasoning"}

		for _, name := range expectedAgents {
			if _, ok := agents[name]; !ok {
				t.Errorf("expected agent %q not generated", name)
			}
		}

		if len(agents) != len(expectedAgents) {
			t.Errorf("expected %d agents, got %d", len(expectedAgents), len(agents))
		}
	})

	// Test with no models matching filter
	t.Run("no models matching filter", func(t *testing.T) {
		emptyCatalog := NewModelCatalog()
		agents := GenerateAutoAgents(emptyCatalog, providers, nil, logger)

		if len(agents) != 0 {
			t.Errorf("expected 0 agents with empty catalog, got %d", len(agents))
		}
	})
}
