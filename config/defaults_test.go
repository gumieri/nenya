package config

import (
	"encoding/json"
	"testing"
)

func TestCompactionPreset_Aggressive(t *testing.T) {
	raw := `{"compaction": {"compaction_preset": "aggressive"}}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	assertCompactionBool(t, cfg.Compaction.JSONMinify, true, "JSONMinify")
	assertCompactionBool(t, cfg.Compaction.CollapseBlankLines, true, "CollapseBlankLines")
	assertCompactionBool(t, cfg.Compaction.TrimTrailingWhitespace, true, "TrimTrailingWhitespace")
	assertCompactionBool(t, cfg.Compaction.NormalizeLineEndings, true, "NormalizeLineEndings")
	assertCompactionBool(t, cfg.Compaction.PruneStaleTools, true, "PruneStaleTools")
	assertCompactionBool(t, cfg.Compaction.PruneThoughts, true, "PruneThoughts")
	if cfg.Compaction.ToolProtectionWindow != 4 {
		t.Errorf("expected ToolProtectionWindow=4, got %d", cfg.Compaction.ToolProtectionWindow)
	}
}

func TestCompactionPreset_Balanced(t *testing.T) {
	raw := `{"compaction": {"compaction_preset": "balanced"}}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	assertCompactionBool(t, cfg.Compaction.JSONMinify, true, "JSONMinify")
	assertCompactionBool(t, cfg.Compaction.CollapseBlankLines, true, "CollapseBlankLines")
	assertCompactionBool(t, cfg.Compaction.TrimTrailingWhitespace, true, "TrimTrailingWhitespace")
	assertCompactionBool(t, cfg.Compaction.NormalizeLineEndings, true, "NormalizeLineEndings")
	assertCompactionBool(t, cfg.Compaction.PruneStaleTools, false, "PruneStaleTools")
	assertCompactionBool(t, cfg.Compaction.PruneThoughts, false, "PruneThoughts")
}

func TestCompactionPreset_Minimal(t *testing.T) {
	raw := `{"compaction": {"compaction_preset": "minimal"}}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	assertCompactionBool(t, cfg.Compaction.JSONMinify, false, "JSONMinify")
	assertCompactionBool(t, cfg.Compaction.CollapseBlankLines, false, "CollapseBlankLines")
	assertCompactionBool(t, cfg.Compaction.TrimTrailingWhitespace, false, "TrimTrailingWhitespace")
	assertCompactionBool(t, cfg.Compaction.NormalizeLineEndings, false, "NormalizeLineEndings")
	assertCompactionBool(t, cfg.Compaction.PruneStaleTools, false, "PruneStaleTools")
	assertCompactionBool(t, cfg.Compaction.PruneThoughts, false, "PruneThoughts")
}

func TestCompactionPreset_WithIndividualOverride(t *testing.T) {
	raw := `{
		"compaction": {
			"compaction_preset": "aggressive",
			"prune_thoughts": false
		}
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	assertCompactionBool(t, cfg.Compaction.JSONMinify, true, "JSONMinify")
	assertCompactionBool(t, cfg.Compaction.CollapseBlankLines, true, "CollapseBlankLines")
	assertCompactionBool(t, cfg.Compaction.TrimTrailingWhitespace, true, "TrimTrailingWhitespace")
	assertCompactionBool(t, cfg.Compaction.NormalizeLineEndings, true, "NormalizeLineEndings")
	assertCompactionBool(t, cfg.Compaction.PruneStaleTools, true, "PruneStaleTools")
	assertCompactionBool(t, cfg.Compaction.PruneThoughts, false, "PruneThoughts")
}

func TestCompactionPreset_UnknownPreset(t *testing.T) {
	raw := `{"compaction": {"compaction_preset": "extreme"}}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	assertCompactionBool(t, cfg.Compaction.JSONMinify, true, "JSONMinify")
	assertCompactionBool(t, cfg.Compaction.CollapseBlankLines, true, "CollapseBlankLines")
	assertCompactionBool(t, cfg.Compaction.TrimTrailingWhitespace, true, "TrimTrailingWhitespace")
	assertCompactionBool(t, cfg.Compaction.NormalizeLineEndings, true, "NormalizeLineEndings")
	assertCompactionBool(t, cfg.Compaction.PruneStaleTools, false, "PruneStaleTools")
	assertCompactionBool(t, cfg.Compaction.PruneThoughts, false, "PruneThoughts")
}

func TestCompactionPreset_EmptyPreset(t *testing.T) {
	raw := `{"compaction": {"compaction_preset": ""}}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	assertCompactionBool(t, cfg.Compaction.JSONMinify, true, "JSONMinify")
	assertCompactionBool(t, cfg.Compaction.CollapseBlankLines, true, "CollapseBlankLines")
	assertCompactionBool(t, cfg.Compaction.TrimTrailingWhitespace, true, "TrimTrailingWhitespace")
	assertCompactionBool(t, cfg.Compaction.NormalizeLineEndings, true, "NormalizeLineEndings")
	assertCompactionBool(t, cfg.Compaction.PruneStaleTools, false, "PruneStaleTools")
	assertCompactionBool(t, cfg.Compaction.PruneThoughts, false, "PruneThoughts")
}

func TestCompactionPreset_EnabledAutoDetection(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		expected bool
	}{
		{
			name:     "aggressive preset enables compaction",
			raw:      `{"compaction": {"compaction_preset": "aggressive"}}`,
			expected: true,
		},
		{
			name:     "minimal preset does not enable compaction",
			raw:      `{"compaction": {"compaction_preset": "minimal"}}`,
			expected: false,
		},
		{
			name:     "balanced preset enables compaction",
			raw:      `{"compaction": {"compaction_preset": "balanced"}}`,
			expected: true,
		},
		{
			name:     "no preset defaults to enabled",
			raw:      `{}`,
			expected: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			if err := json.Unmarshal([]byte(tt.raw), &cfg); err != nil {
				t.Fatal(err)
			}
			if err := ApplyDefaults(&cfg); err != nil {
				t.Fatal(err)
			}
			if tt.expected {
				if cfg.Compaction.Enabled == nil || !*cfg.Compaction.Enabled {
					t.Errorf("expected compaction enabled, got %v", cfg.Compaction.Enabled)
				}
			} else {
				if cfg.Compaction.Enabled != nil && *cfg.Compaction.Enabled {
					t.Errorf("expected compaction disabled, got %v", cfg.Compaction.Enabled)
				}
			}
		})
	}
}

func assertCompactionBool(t *testing.T, got *bool, want bool, name string) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: got nil, want %v", name, want)
		return
	}
	if *got != want {
		t.Errorf("%s: got %v, want %v", name, *got, want)
	}
}
