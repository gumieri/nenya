package proxy

import (
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/pipeline"
)

func TestSpotlightSettingsFor(t *testing.T) {
	enabled := true
	cfgNoSpotlight := config.Config{}
	cfgEnabled := config.Config{Governance: config.GovernanceConfig{
		Spotlight: &config.SpotlightConfig{Enabled: &enabled, Mode: config.SpotlightModeDatamarking, MaxToolResultBytes: 128},
	}}
	agentOverride := config.AgentConfig{Spotlight: &config.SpotlightConfig{Enabled: func() *bool { v := false; return &v }()}}
	agentOptIn := config.AgentConfig{Spotlight: &config.SpotlightConfig{Enabled: &enabled}}

	t.Run("nil global and nil agent disabled", func(t *testing.T) {
		got := spotlightSettingsFor(&cfgNoSpotlight, config.AgentConfig{})
		if got.Enabled {
			t.Error("expected disabled")
		}
	})

	t.Run("nil global with agent opt-in materializes defaults", func(t *testing.T) {
		got := spotlightSettingsFor(&cfgNoSpotlight, agentOptIn)
		if !got.Enabled {
			t.Error("expected enabled via agent override")
		}
		if got.Mode != pipeline.SpotlightModeDelimiters || got.MaxToolResultBytes != config.DefaultMaxToolResultBytes {
			t.Errorf("expected delimiters + default cap, got %+v", got)
		}
	})

	t.Run("global settings win for plain agents", func(t *testing.T) {
		got := spotlightSettingsFor(&cfgEnabled, config.AgentConfig{})
		if !got.Enabled || got.Mode != pipeline.SpotlightModeDatamarking || got.MaxToolResultBytes != 128 {
			t.Errorf("unexpected settings %+v", got)
		}
	})

	t.Run("agent disable overrides global", func(t *testing.T) {
		got := spotlightSettingsFor(&cfgEnabled, agentOverride)
		if got.Enabled {
			t.Error("expected agent disable to win")
		}
		if got.Mode != pipeline.SpotlightModeDatamarking {
			t.Errorf("global-only mode must be preserved, got %q", got.Mode)
		}
	})
}

func TestApplyToolDescriptionSpotlight(t *testing.T) {
	settings := pipeline.SpotlightSettings{Enabled: true, MaxToolResultBytes: 128}
	tools := []map[string]any{
		{"function": map[string]any{"name": "ok", "description": strings.Repeat("d", 300)}},
		{"function": map[string]any{"name": "no-desc"}},
		{"function": map[string]any{"name": "bad-desc", "description": 42}},
		{"not-function": true},
	}
	applyToolDescriptionSpotlight(tools, settings, nil)

	fn := tools[0]["function"].(map[string]any)
	desc := fn["description"].(string)
	if !strings.HasPrefix(desc, descriptionSpotlightPrefix) {
		t.Errorf("expected untrusted prefix, got %q", desc)
	}
	if !strings.Contains(desc, "TRUNCATED") {
		t.Errorf("expected cap truncation, got %q", desc)
	}
	if len(desc) > len(descriptionSpotlightPrefix)+128 {
		t.Errorf("expected raw description capped at 128 bytes, got %d", len(desc))
	}
	if desc, has := tools[1]["function"].(map[string]any)["description"]; has && desc != "" {
		t.Errorf("empty description should stay empty, got %v", desc)
	}
	if v, has := tools[2]["function"].(map[string]any)["description"]; !has || v != 42 {
		t.Errorf("non-string description should be untouched, got %v", v)
	}

	disabled := pipeline.SpotlightSettings{}
	before := tools[0]["function"].(map[string]any)["description"]
	applyToolDescriptionSpotlight(tools, disabled, nil)
	if tools[0]["function"].(map[string]any)["description"] != before {
		t.Error("disabled settings must be a no-op")
	}
}
