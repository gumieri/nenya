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
			},
		},
		{
			name: "non-zero values preserved",
			before: Config{
				Server: ServerConfig{
					ListenAddr:   ":9090",
					MaxBodyBytes: 1 << 20,
				},
			},
			check: func(t *testing.T, c *Config) {
				if c.Server.ListenAddr != ":9090" {
					t.Errorf("ListenAddr: got %q", c.Server.ListenAddr)
				}
				if c.Server.MaxBodyBytes != 1<<20 {
					t.Errorf("MaxBodyBytes: got %d", c.Server.MaxBodyBytes)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ApplyDefaults(&tt.before); err != nil {
				t.Fatal(err)
			}
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
					KeepFirstPercent: 30.0,
				},
			},
			check: func(t *testing.T, c *Config) {
				if c.Governance.KeepFirstPercent != 30.0 {
					t.Errorf("KeepFirstPercent: got %f", c.Governance.KeepFirstPercent)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ApplyDefaults(&tt.before); err != nil {
				t.Fatal(err)
			}
			tt.check(t, &tt.before)
		})
	}
}

func TestApplyDefaultsSecurityFilterEngine(t *testing.T) {
	cfg := Config{}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatal(err)
	}

	if len(cfg.SecurityFilter.Engine.ResolvedTargets) == 0 {
		t.Fatal("expected at least one resolved target")
	}
	target := cfg.SecurityFilter.Engine.ResolvedTargets[0]
	if target.Engine.Provider != "ollama" {
		t.Errorf("Engine.Provider: got %q", target.Engine.Provider)
	}
	if target.Engine.Model != "qwen2.5-coder:7b" {
		t.Errorf("Engine.Model: got %q", target.Engine.Model)
	}
	if target.Engine.TimeoutSeconds != 60 {
		t.Errorf("Engine.TimeoutSeconds: got %d", target.Engine.TimeoutSeconds)
	}
}

func TestApplyDefaultsSecurityFilter(t *testing.T) {
	t.Run("empty gets built-ins", func(t *testing.T) {
		cfg := Config{}
		if err := ApplyDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
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
		if err := ApplyDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
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
		if err := ApplyDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.SecurityFilter.Enabled {
			t.Error("expected Enabled=false when explicitly set")
		}
	})
}

func TestApplyDefaultsPrefixCache(t *testing.T) {
	t.Run("sub-field defaults", func(t *testing.T) {
		cfg := Config{}
		if err := ApplyDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if !cfg.PrefixCache.PinSystemFirst {
			t.Error("expected PinSystemFirst=true")
		}
		if !cfg.PrefixCache.StableTools {
			t.Error("expected StableTools=true")
		}
	})
	t.Run("auto-enable parent", func(t *testing.T) {
		cfg := Config{PrefixCache: PrefixCacheConfig{PinSystemFirst: true}}
		if err := ApplyDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
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
		if err := ApplyDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.PrefixCache.PinSystemFirst {
			t.Error("expected PinSystemFirst=false")
		}
	})
}

func TestApplyDefaultsCompaction(t *testing.T) {
	t.Run("auto-enable", func(t *testing.T) {
		cfg := Config{}
		if err := ApplyDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if !cfg.Compaction.Enabled {
			t.Error("expected Enabled=true")
		}
	})
	t.Run("sub-fields default true", func(t *testing.T) {
		cfg := Config{}
		if err := ApplyDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
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
		if err := ApplyDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Compaction.JSONMinify {
			t.Error("expected JSONMinify=false")
		}
	})
}

func TestApplyDefaultsWindow(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg := Config{}
		if err := ApplyDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
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
		if err := ApplyDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if !cfg.Window.Enabled {
			t.Error("expected Enabled=true when Mode set")
		}
	})
}

func TestApplyDefaultsProviders(t *testing.T) {
	t.Run("nil gets built-ins", func(t *testing.T) {
		cfg := Config{}
		if err := ApplyDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
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
		if err := ApplyDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Providers["gemini"].URL != "http://custom" {
			t.Errorf("expected custom URL, got %q", cfg.Providers["gemini"].URL)
		}
	})
}

func TestLoadConfig(t *testing.T) {
	t.Run("valid JSON", func(t *testing.T) {
		tmpfile := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(tmpfile, []byte(`{"server":{"listen_addr":":9090"}}`), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
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
		if err := os.WriteFile(tmpfile, []byte(`{invalid}`), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := Load(tmpfile)
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("comments stripped", func(t *testing.T) {
		tmpfile := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(tmpfile, []byte(`{"server": {"listen_addr": ":9090" /* trailing */}}`), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
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
		if err := os.WriteFile(filepath.Join(dir, "secrets"),
			[]byte(`{"client_token":"tok","provider_keys":{"gemini":"key1"}}`), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
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
		if err := os.WriteFile(filepath.Join(dir, "secrets"),
			[]byte(`{"provider_keys":{}}`), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
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
		if err := os.WriteFile(tmpfile, []byte("file content"), 0644); err != nil {
			t.Fatal(err)
		}
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

func TestLoadDirectory(t *testing.T) {
	t.Run("multiple files merged", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "00-server.json"), []byte(`{"server":{"listen_addr":":9090"}}`), 0644); err != nil {
			t.Fatalf("failed to create 00-server.json: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "10-governance.json"), []byte(`{"governance":{"truncation_strategy":"middle-out"}}`), 0644); err != nil {
			t.Fatalf("failed to create 10-governance.json: %v", err)
		}

		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.Server.ListenAddr != ":9090" {
			t.Errorf("ListenAddr: got %q", cfg.Server.ListenAddr)
		}
		if cfg.Governance.TruncationStrategy != "middle-out" {
			t.Errorf("TruncationStrategy: got %q", cfg.Governance.TruncationStrategy)
		}
	})

	t.Run("sorted alphabetically", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "20-later.json"), []byte(`{"server":{"listen_addr":":9090"}}`), 0644); err != nil {
			t.Fatalf("failed to create 20-later.json: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "10-earlier.json"), []byte(`{"server":{"listen_addr":":8080"}}`), 0644); err != nil {
			t.Fatalf("failed to create 10-earlier.json: %v", err)
		}

		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.Server.ListenAddr != ":9090" {
			t.Errorf("expected later file to win, got ListenAddr %q", cfg.Server.ListenAddr)
		}
	})

	t.Run("excludes secrets.json", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(`{"server":{"listen_addr":":9999"}}`), 0644); err != nil {
			t.Fatalf("failed to create secrets.json: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"server":{"listen_addr":":8080"}}`), 0644); err != nil {
			t.Fatalf("failed to create config.json: %v", err)
		}

		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.Server.ListenAddr == ":9999" {
			t.Error("secrets.json should be excluded from merge")
		}
	})

	t.Run("map fields merge per-key", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "10-providers.json"), []byte(`{
			"providers": {
				"gemini": {"url": "http://gemini", "route_prefixes": ["gemini-"]},
				"deepseek": {"url": "http://deepseek", "route_prefixes": ["deepseek-"]}
			}
		}`), 0644); err != nil {
			t.Fatalf("failed to create 10-providers.json: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "20-providers-override.json"), []byte(`{
			"providers": {
				"gemini": {"url": "http://custom-gemini", "route_prefixes": ["gemini-"]}
			}
		}`), 0644); err != nil {
			t.Fatalf("failed to create 20-providers-override.json: %v", err)
		}

		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.Providers["gemini"].URL != "http://custom-gemini" {
			t.Errorf("gemini URL: got %q", cfg.Providers["gemini"].URL)
		}
		if cfg.Providers["deepseek"].URL != "http://deepseek" {
			t.Errorf("deepseek should be preserved: got %q", cfg.Providers["deepseek"].URL)
		}
	})

	t.Run("agents map merge", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "10-agents.json"), []byte(`{
			"agents": {
				"coder": {"models": [{"provider":"gemini","model":"gemini-2.5-pro"}]}
			}
		}`), 0644); err != nil {
			t.Fatalf("failed to create 10-agents.json: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "20-agents-extra.json"), []byte(`{
			"agents": {
				"researcher": {"models": [{"provider":"gemini","model":"gemini-2.5-flash"}]}
			}
		}`), 0644); err != nil {
			t.Fatalf("failed to create 20-agents-extra.json: %v", err)
		}

		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if _, ok := cfg.Agents["coder"]; !ok {
			t.Error("expected coder agent from first file")
		}
		if _, ok := cfg.Agents["researcher"]; !ok {
			t.Error("expected researcher agent from second file")
		}
	})

	t.Run("empty directory returns error", func(t *testing.T) {
		dir := t.TempDir()
		_, err := Load(dir)
		if err == nil {
			t.Fatal("expected error for empty directory")
		}
	})

	t.Run("non-existent directory returns error", func(t *testing.T) {
		_, err := Load("/nonexistent/dir")
		if err == nil {
			t.Fatal("expected error for non-existent directory")
		}
	})

	t.Run("defaults applied after merge", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{}`), 0644); err != nil {
			t.Fatalf("failed to create config.json: %v", err)
		}

		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.Server.ListenAddr != ":8080" {
			t.Errorf("expected default ListenAddr, got %q", cfg.Server.ListenAddr)
		}
		if cfg.Governance.TruncationStrategy != "middle-out" {
			t.Errorf("expected default TruncationStrategy, got %q", cfg.Governance.TruncationStrategy)
		}
	})

	t.Run("single file still works", func(t *testing.T) {
		tmpfile := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(tmpfile, []byte(`{"server":{"listen_addr":":9090"}}`), 0644); err != nil {
			t.Fatalf("failed to create config.json: %v", err)
		}

		cfg, err := Load(tmpfile)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.Server.ListenAddr != ":9090" {
			t.Errorf("ListenAddr: got %q", cfg.Server.ListenAddr)
		}
	})

	t.Run("non-json files ignored", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("not json"), 0644); err != nil {
			t.Fatalf("failed to create README.md: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"server":{"listen_addr":":9090"}}`), 0644); err != nil {
			t.Fatalf("failed to create config.json: %v", err)
		}

		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.Server.ListenAddr != ":9090" {
			t.Errorf("ListenAddr: got %q", cfg.Server.ListenAddr)
		}
	})
}

func TestModelRegistry_GLM5MaxOutput(t *testing.T) {
	glm5Models := []string{"glm-5.1", "glm-5-turbo", "glm-5v-turbo", "glm-5"}
	for _, model := range glm5Models {
		entry, ok := ModelRegistry[model]
		if !ok {
			t.Fatalf("model %q missing from ModelRegistry", model)
		}
		if entry.MaxOutput < 16384 {
			t.Errorf("model %q: MaxOutput=%d, want >= 16384 (tool use requires larger output budget)", model, entry.MaxOutput)
		}
		if entry.Provider != "zai" {
			t.Errorf("model %q: Provider=%q, want zai", model, entry.Provider)
		}
	}
}
