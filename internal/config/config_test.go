package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyDefaultsServer(t *testing.T) {
	tests := []struct {
		name   string
		before Config
		check  func(t *testing.T, c *Config)
	}{
		{
			name:   "zero values get defaults",
			before: Config{},
			check: func(t *testing.T, c *Config) {
				if c.Server.ListenAddr != ":8080" {
					t.Errorf("ListenAddr: got %q", c.Server.ListenAddr)
				}
				if c.Server.MaxBodyBytes != 10<<20 {
					t.Errorf("MaxBodyBytes: got %d", c.Server.MaxBodyBytes)
				}
				if c.Server.TokenRatio != 4.0 {
					t.Errorf("TokenRatio: got %f", c.Server.TokenRatio)
				}
			},
		},
		{
			name: "non-zero values preserved",
			before: Config{
				Server: ServerConfig{
					ListenAddr:   ":9090",
					MaxBodyBytes: 1 << 20,
					TokenRatio:   2.5,
				},
			},
			check: func(t *testing.T, c *Config) {
				if c.Server.ListenAddr != ":9090" {
					t.Errorf("ListenAddr: got %q", c.Server.ListenAddr)
				}
				if c.Server.MaxBodyBytes != 1<<20 {
					t.Errorf("MaxBodyBytes: got %d", c.Server.MaxBodyBytes)
				}
				if c.Server.TokenRatio != 2.5 {
					t.Errorf("TokenRatio: got %f", c.Server.TokenRatio)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ApplyDefaults(&tt.before)
			tt.check(t, &tt.before)
		})
	}
}

func TestApplyDefaultsGovernance(t *testing.T) {
	tests := []struct {
		name   string
		before Config
		check  func(t *testing.T, c *Config)
	}{
		{
			name:   "zero values get defaults",
			before: Config{},
			check: func(t *testing.T, c *Config) {
				if c.Governance.ContextSoftLimit != 4000 {
					t.Errorf("ContextSoftLimit: got %d", c.Governance.ContextSoftLimit)
				}
				if c.Governance.ContextHardLimit != 24000 {
					t.Errorf("ContextHardLimit: got %d", c.Governance.ContextHardLimit)
				}
				if c.Governance.TruncationStrategy != "middle-out" {
					t.Errorf("TruncationStrategy: got %q", c.Governance.TruncationStrategy)
				}
				if c.Governance.KeepFirstPercent != 15.0 {
					t.Errorf("KeepFirstPercent: got %f", c.Governance.KeepFirstPercent)
				}
				if c.Governance.KeepLastPercent != 25.0 {
					t.Errorf("KeepLastPercent: got %f", c.Governance.KeepLastPercent)
				}
			},
		},
		{
			name: "explicit values preserved",
			before: Config{
				Governance: GovernanceConfig{
					ContextSoftLimit: 1000,
					KeepFirstPercent: 30.0,
				},
			},
			check: func(t *testing.T, c *Config) {
				if c.Governance.ContextSoftLimit != 1000 {
					t.Errorf("ContextSoftLimit: got %d", c.Governance.ContextSoftLimit)
				}
				if c.Governance.KeepFirstPercent != 30.0 {
					t.Errorf("KeepFirstPercent: got %f", c.Governance.KeepFirstPercent)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ApplyDefaults(&tt.before)
			tt.check(t, &tt.before)
		})
	}
}

func TestApplyDefaultsSecurityFilterEngine(t *testing.T) {
	cfg := Config{}
	ApplyDefaults(&cfg)

	if cfg.SecurityFilter.Engine.Provider != "ollama" {
		t.Errorf("Engine.Provider: got %q", cfg.SecurityFilter.Engine.Provider)
	}
	if cfg.SecurityFilter.Engine.Model != "qwen2.5-coder:7b" {
		t.Errorf("Engine.Model: got %q", cfg.SecurityFilter.Engine.Model)
	}
	if cfg.SecurityFilter.Engine.TimeoutSeconds != 600 {
		t.Errorf("Engine.TimeoutSeconds: got %d", cfg.SecurityFilter.Engine.TimeoutSeconds)
	}
}

func TestApplyDefaultsSecurityFilter(t *testing.T) {
	t.Run("empty gets built-ins", func(t *testing.T) {
		cfg := Config{}
		ApplyDefaults(&cfg)
		if !cfg.SecurityFilter.Enabled {
			t.Error("expected Enabled=true")
		}
		if len(cfg.SecurityFilter.Patterns) == 0 {
			t.Error("expected default patterns")
		}
	})
	t.Run("patterns preserved", func(t *testing.T) {
		cfg := Config{
			SecurityFilter: SecurityFilterConfig{
				Patterns: []string{`custom-[0-9]+`},
			},
		}
		ApplyDefaults(&cfg)
		if len(cfg.SecurityFilter.Patterns) != 1 {
			t.Errorf("expected 1 pattern, got %d", len(cfg.SecurityFilter.Patterns))
		}
	})
	t.Run("enabled=false via JSON", func(t *testing.T) {
		jsonStr := `{"security_filter":{"enabled":false,"patterns":["test"]}}`
		var cfg Config
		if err := json.Unmarshal([]byte(jsonStr), &cfg); err != nil {
			t.Fatal(err)
		}
		ApplyDefaults(&cfg)
		if cfg.SecurityFilter.Enabled {
			t.Error("expected Enabled=false when explicitly set")
		}
	})
}

func TestApplyDefaultsPrefixCache(t *testing.T) {
	t.Run("sub-field defaults", func(t *testing.T) {
		cfg := Config{}
		ApplyDefaults(&cfg)
		if !cfg.PrefixCache.PinSystemFirst {
			t.Error("expected PinSystemFirst=true")
		}
		if !cfg.PrefixCache.StableTools {
			t.Error("expected StableTools=true")
		}
	})
	t.Run("auto-enable parent", func(t *testing.T) {
		cfg := Config{PrefixCache: PrefixCacheConfig{PinSystemFirst: true}}
		ApplyDefaults(&cfg)
		if !cfg.PrefixCache.Enabled {
			t.Error("expected Enabled=true when sub-fields set")
		}
	})
	t.Run("explicit false preserved via JSON", func(t *testing.T) {
		jsonStr := `{"prefix_cache":{"pin_system_first":false}}`
		var cfg Config
		if err := json.Unmarshal([]byte(jsonStr), &cfg); err != nil {
			t.Fatal(err)
		}
		ApplyDefaults(&cfg)
		if cfg.PrefixCache.PinSystemFirst {
			t.Error("expected PinSystemFirst=false")
		}
	})
}

func TestApplyDefaultsCompaction(t *testing.T) {
	t.Run("auto-enable", func(t *testing.T) {
		cfg := Config{}
		ApplyDefaults(&cfg)
		if !cfg.Compaction.Enabled {
			t.Error("expected Enabled=true")
		}
	})
	t.Run("sub-fields default true", func(t *testing.T) {
		cfg := Config{}
		ApplyDefaults(&cfg)
		if !cfg.Compaction.JSONMinify {
			t.Error("expected JSONMinify=true")
		}
		if !cfg.Compaction.CollapseBlankLines {
			t.Error("expected CollapseBlankLines=true")
		}
	})
	t.Run("explicit false preserved via JSON", func(t *testing.T) {
		jsonStr := `{"compaction":{"json_minify":false}}`
		var cfg Config
		if err := json.Unmarshal([]byte(jsonStr), &cfg); err != nil {
			t.Fatal(err)
		}
		ApplyDefaults(&cfg)
		if cfg.Compaction.JSONMinify {
			t.Error("expected JSONMinify=false")
		}
	})
}

func TestApplyDefaultsWindow(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg := Config{}
		ApplyDefaults(&cfg)
		if cfg.Window.Mode != "summarize" {
			t.Errorf("Mode: got %q", cfg.Window.Mode)
		}
		if cfg.Window.ActiveMessages != 6 {
			t.Errorf("ActiveMessages: got %d", cfg.Window.ActiveMessages)
		}
		if cfg.Window.TriggerRatio != 0.8 {
			t.Errorf("TriggerRatio: got %f", cfg.Window.TriggerRatio)
		}
		if cfg.Window.SummaryMaxRunes != 4000 {
			t.Errorf("SummaryMaxRunes: got %d", cfg.Window.SummaryMaxRunes)
		}
	})
	t.Run("auto-enable", func(t *testing.T) {
		cfg := Config{Window: WindowConfig{Mode: "truncate"}}
		ApplyDefaults(&cfg)
		if !cfg.Window.Enabled {
			t.Error("expected Enabled=true when Mode set")
		}
	})
}

func TestApplyDefaultsProviders(t *testing.T) {
	t.Run("nil gets built-ins", func(t *testing.T) {
		cfg := Config{}
		ApplyDefaults(&cfg)
		if _, ok := cfg.Providers["gemini"]; !ok {
			t.Error("expected built-in gemini provider")
		}
	})
	t.Run("user override wins", func(t *testing.T) {
		cfg := Config{
			Providers: map[string]ProviderConfig{
				"gemini": {URL: "http://custom"},
			},
		}
		ApplyDefaults(&cfg)
		if cfg.Providers["gemini"].URL != "http://custom" {
			t.Errorf("expected custom URL, got %q", cfg.Providers["gemini"].URL)
		}
	})
}

func TestLoadConfig(t *testing.T) {
	t.Run("valid JSON", func(t *testing.T) {
		tmpfile := filepath.Join(t.TempDir(), "config.json")
		os.WriteFile(tmpfile, []byte(`{"server":{"listen_addr":":9090"}}`), 0644)
		cfg, err := Load(tmpfile)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.Server.ListenAddr != ":9090" {
			t.Errorf("ListenAddr: got %q", cfg.Server.ListenAddr)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		_, err := Load("/nonexistent/config.json")
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("invalid JSON", func(t *testing.T) {
		tmpfile := filepath.Join(t.TempDir(), "config.json")
		os.WriteFile(tmpfile, []byte(`{invalid}`), 0644)
		_, err := Load(tmpfile)
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("comments stripped", func(t *testing.T) {
		tmpfile := filepath.Join(t.TempDir(), "config.json")
		os.WriteFile(tmpfile, []byte(`{"server": {"listen_addr": ":9090" /* trailing */}}`), 0644)
		cfg, err := Load(tmpfile)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.Server.ListenAddr != ":9090" {
			t.Errorf("ListenAddr: got %q", cfg.Server.ListenAddr)
		}
	})
}

func TestLoadSecrets(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("CREDENTIALS_DIRECTORY", dir)
		os.WriteFile(filepath.Join(dir, "secrets"),
			[]byte(`{"client_token":"tok","provider_keys":{"gemini":"key1"}}`), 0644)
		s, err := LoadSecrets()
		if err != nil {
			t.Fatalf("LoadSecrets failed: %v", err)
		}
		if s.ClientToken != "tok" {
			t.Errorf("ClientToken: got %q", s.ClientToken)
		}
	})
	t.Run("missing env", func(t *testing.T) {
		t.Setenv("CREDENTIALS_DIRECTORY", "")
		_, err := LoadSecrets()
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("missing client_token", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("CREDENTIALS_DIRECTORY", dir)
		os.WriteFile(filepath.Join(dir, "secrets"),
			[]byte(`{"provider_keys":{}}`), 0644)
		_, err := LoadSecrets()
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestResolveProviders(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"gemini": {URL: "http://gemini", RoutePrefixes: []string{"gemini-"}},
		},
	}
	secrets := &SecretsConfig{ProviderKeys: map[string]string{"gemini": "key1"}}

	p := ResolveProviders(cfg, secrets)
	if len(p) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(p))
	}
	if p["gemini"].APIKey != "key1" {
		t.Errorf("APIKey: got %q", p["gemini"].APIKey)
	}
	if p["gemini"].URL != "http://gemini" {
		t.Errorf("URL: got %q", p["gemini"].URL)
	}
}

func TestBuiltInProviders(t *testing.T) {
	p := BuiltInProviders()
	if _, ok := p["gemini"]; !ok {
		t.Error("expected built-in gemini")
	}
	if _, ok := p["deepseek"]; !ok {
		t.Error("expected built-in deepseek")
	}
	if _, ok := p["zai"]; !ok {
		t.Error("expected built-in zai")
	}
}

func TestLoadPromptFile(t *testing.T) {
	t.Run("direct prompt", func(t *testing.T) {
		s, err := LoadPromptFile("", "hello", "default")
		if err != nil {
			t.Fatal(err)
		}
		if s != "hello" {
			t.Errorf("got %q", s)
		}
	})
	t.Run("file prompt", func(t *testing.T) {
		tmpfile := filepath.Join(t.TempDir(), "prompt.md")
		os.WriteFile(tmpfile, []byte("file content"), 0644)
		s, err := LoadPromptFile(tmpfile, "", "default")
		if err != nil {
			t.Fatal(err)
		}
		if s != "file content" {
			t.Errorf("got %q", s)
		}
	})
	t.Run("default prompt", func(t *testing.T) {
		s, err := LoadPromptFile("", "", "default value")
		if err != nil {
			t.Fatal(err)
		}
		if s != "default value" {
			t.Errorf("got %q", s)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		_, err := LoadPromptFile("/nonexistent.md", "", "default")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}
