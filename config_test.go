package main

import (
	"encoding/json"
	"net/http"
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
			cfg := tt.before
			applyDefaults(&cfg)
			tt.check(t, &cfg)
		})
	}
}

func TestExampleConfig(t *testing.T) {
	cfg, err := loadConfig("example.config.json")
	if err != nil {
		t.Fatalf("failed to load example.config.json: %v", err)
	}

	// Validate some expected values from example.config.json
	if cfg.Server.ListenAddr != ":8080" {
		t.Errorf("Server.ListenAddr: got %q, want :8080", cfg.Server.ListenAddr)
	}
	if cfg.Governance.RatelimitMaxTPM != 250000 {
		t.Errorf("Governance.RatelimitMaxTPM: got %d, want 250000", cfg.Governance.RatelimitMaxTPM)
	}
	if !cfg.SecurityFilter.Enabled {
		t.Error("SecurityFilter.Enabled should be true")
	}
	if cfg.SecurityFilter.Engine.Model != "qwen2.5-coder:7b" {
		t.Errorf("SecurityFilter.Engine.Model: got %q, want qwen2.5-coder:7b", cfg.SecurityFilter.Engine.Model)
	}
	if cfg.Window.Engine.Model != "qwen2.5-coder:7b" {
		t.Errorf("Window.Engine.Model: got %q, want qwen2.5-coder:7b", cfg.Window.Engine.Model)
	}
	if len(cfg.Providers) < 2 {
		t.Errorf("Providers count: got %d, want at least 2", len(cfg.Providers))
	}
	if cfg.Providers["openai"].URL != "https://api.openai.com/v1/chat/completions" {
		t.Errorf("openai URL: got %q", cfg.Providers["openai"].URL)
	}
	if cfg.Providers["zai"].URL != "https://api.z.ai/v1/chat/completions" {
		t.Errorf("zai URL: got %q", cfg.Providers["zai"].URL)
	}
}
func TestApplyDefaultsGovernance(t *testing.T) {
	cfg := Config{}
	applyDefaults(&cfg)

	if cfg.Governance.ContextSoftLimit != 4000 {
		t.Errorf("ContextSoftLimit: got %d", cfg.Governance.ContextSoftLimit)
	}
	if cfg.Governance.ContextHardLimit != 24000 {
		t.Errorf("ContextHardLimit: got %d", cfg.Governance.ContextHardLimit)
	}
	if cfg.Governance.TruncationStrategy != "middle-out" {
		t.Errorf("TruncationStrategy: got %q", cfg.Governance.TruncationStrategy)
	}
	if cfg.Governance.KeepFirstPercent != 15.0 {
		t.Errorf("KeepFirstPercent: got %f", cfg.Governance.KeepFirstPercent)
	}
	if cfg.Governance.KeepLastPercent != 25.0 {
		t.Errorf("KeepLastPercent: got %f", cfg.Governance.KeepLastPercent)
	}

	cfg2 := Config{
		Governance: GovernanceConfig{
			ContextSoftLimit:   8000,
			ContextHardLimit:   48000,
			TruncationStrategy: "head-tail",
			KeepFirstPercent:   30.0,
			KeepLastPercent:    10.0,
		},
	}
	applyDefaults(&cfg2)
	if cfg2.Governance.ContextSoftLimit != 8000 {
		t.Errorf("ContextSoftLimit preserved: got %d", cfg2.Governance.ContextSoftLimit)
	}
	if cfg2.Governance.ContextHardLimit != 48000 {
		t.Errorf("ContextHardLimit preserved: got %d", cfg2.Governance.ContextHardLimit)
	}
	if cfg2.Governance.TruncationStrategy != "head-tail" {
		t.Errorf("TruncationStrategy preserved: got %q", cfg2.Governance.TruncationStrategy)
	}
}

func TestApplyDefaultsSecurityFilterEngine(t *testing.T) {
	cfg := Config{}
	applyDefaults(&cfg)

	if cfg.SecurityFilter.Engine.URL != "http://127.0.0.1:11434/api/generate" {
		t.Errorf("URL: got %q", cfg.SecurityFilter.Engine.URL)
	}
	if cfg.SecurityFilter.Engine.Model != "qwen2.5-coder:7b" {
		t.Errorf("Model: got %q", cfg.SecurityFilter.Engine.Model)
	}
	if cfg.SecurityFilter.Engine.ApiFormat != "ollama" {
		t.Errorf("ApiFormat: got %q", cfg.SecurityFilter.Engine.ApiFormat)
	}
	if cfg.SecurityFilter.Engine.TimeoutSeconds != 600 {
		t.Errorf("TimeoutSeconds: got %d", cfg.SecurityFilter.Engine.TimeoutSeconds)
	}

	cfg2 := Config{
		SecurityFilter: SecurityFilterConfig{
			Engine: EngineConfig{
				URL:            "http://localhost:11434/api/generate",
				Model:          "llama3:8b",
				ApiFormat:      "openai",
				TimeoutSeconds: 120,
			},
		},
	}
	applyDefaults(&cfg2)
	if cfg2.SecurityFilter.Engine.URL != "http://localhost:11434/api/generate" {
		t.Errorf("URL preserved: got %q", cfg2.SecurityFilter.Engine.URL)
	}
	if cfg2.SecurityFilter.Engine.Model != "llama3:8b" {
		t.Errorf("Model preserved: got %q", cfg2.SecurityFilter.Engine.Model)
	}
	if cfg2.SecurityFilter.Engine.ApiFormat != "openai" {
		t.Errorf("ApiFormat preserved: got %q", cfg2.SecurityFilter.Engine.ApiFormat)
	}
	if cfg2.SecurityFilter.Engine.TimeoutSeconds != 120 {
		t.Errorf("TimeoutSeconds preserved: got %d", cfg2.SecurityFilter.Engine.TimeoutSeconds)
	}
}

func TestApplyDefaultsSecurityFilter(t *testing.T) {
	t.Run("empty config gets built-in patterns", func(t *testing.T) {
		cfg := Config{}
		applyDefaults(&cfg)

		if !cfg.SecurityFilter.Enabled {
			t.Error("SecurityFilter.Enabled should default to true")
		}
		if cfg.SecurityFilter.RedactionLabel != "[REDACTED]" {
			t.Errorf("RedactionLabel: got %q", cfg.SecurityFilter.RedactionLabel)
		}
		if len(cfg.SecurityFilter.Patterns) == 0 {
			t.Error("Patterns: expected built-in patterns")
		}
	})

	t.Run("patterns provided are preserved", func(t *testing.T) {
		cfg := Config{
			SecurityFilter: SecurityFilterConfig{
				enabledSet:     true,
				Enabled:        false,
				RedactionLabel: "X",
				Patterns:       []string{"a", "b"},
			},
		}
		applyDefaults(&cfg)

		if cfg.SecurityFilter.Enabled {
			t.Error("SecurityFilter.Enabled should stay false when explicitly set")
		}
		if cfg.SecurityFilter.RedactionLabel != "X" {
			t.Errorf("RedactionLabel preserved: got %q", cfg.SecurityFilter.RedactionLabel)
		}
		if len(cfg.SecurityFilter.Patterns) != 2 || cfg.SecurityFilter.Patterns[0] != "a" || cfg.SecurityFilter.Patterns[1] != "b" {
			t.Errorf("Patterns preserved: got %v", cfg.SecurityFilter.Patterns)
		}
	})

	t.Run("enabled stays false when patterns set but enabled not set", func(t *testing.T) {
		cfg := Config{
			SecurityFilter: SecurityFilterConfig{
				RedactionLabel: "X",
				Patterns:       []string{"c"},
			},
		}
		applyDefaults(&cfg)

		if !cfg.SecurityFilter.Enabled {
			t.Error("SecurityFilter.Enabled should default to true when patterns set but enabled not set")
		}
		if cfg.SecurityFilter.RedactionLabel != "X" {
			t.Errorf("RedactionLabel preserved: got %q", cfg.SecurityFilter.RedactionLabel)
		}
	})
}

func TestApplyDefaultsPrefixCache(t *testing.T) {
	t.Run("zero config: Enabled stays false but sub-fields get defaults", func(t *testing.T) {
		cfg := Config{}
		applyDefaults(&cfg)

		if cfg.PrefixCache.Enabled {
			t.Error("PrefixCache.Enabled stays false for zero config (precedence in condition)")
		}
		if !cfg.PrefixCache.PinSystemFirst {
			t.Error("PinSystemFirst should default to true")
		}
		if !cfg.PrefixCache.StableTools {
			t.Error("StableTools should default to true")
		}
		if !cfg.PrefixCache.SkipRedactionOnSystem {
			t.Error("SkipRedactionOnSystem should default to true")
		}
	})

	t.Run("sub-field enables parent", func(t *testing.T) {
		cfg := Config{
			PrefixCache: PrefixCacheConfig{PinSystemFirst: true},
		}
		applyDefaults(&cfg)

		if !cfg.PrefixCache.Enabled {
			t.Error("setting PinSystemFirst should auto-enable PrefixCache")
		}
	})
}

func TestApplyDefaultsCompaction(t *testing.T) {
	t.Run("zero config: Enabled stays false but sub-fields get defaults", func(t *testing.T) {
		cfg := Config{}
		applyDefaults(&cfg)

		if cfg.Compaction.Enabled {
			t.Error("Compaction.Enabled stays false for zero config")
		}
		if !cfg.Compaction.JSONMinify {
			t.Error("JSONMinify should default to true")
		}
		if !cfg.Compaction.CollapseBlankLines {
			t.Error("CollapseBlankLines should default to true")
		}
		if !cfg.Compaction.TrimTrailingWhitespace {
			t.Error("TrimTrailingWhitespace should default to true")
		}
		if !cfg.Compaction.NormalizeLineEndings {
			t.Error("NormalizeLineEndings should default to true")
		}
	})

	t.Run("sub-field enables parent", func(t *testing.T) {
		cfg := Config{
			Compaction: CompactionConfig{NormalizeLineEndings: true},
		}
		applyDefaults(&cfg)

		if !cfg.Compaction.Enabled {
			t.Error("setting NormalizeLineEndings should auto-enable Compaction")
		}
	})

	t.Run("user false values get overridden by defaults", func(t *testing.T) {
		cfg := Config{
			Compaction: CompactionConfig{
				Enabled:            true,
				JSONMinify:         false,
				CollapseBlankLines: false,
			},
		}
		applyDefaults(&cfg)

		if !cfg.Compaction.JSONMinify {
			t.Error("JSONMinify=false gets overridden to true by default logic")
		}
		if !cfg.Compaction.CollapseBlankLines {
			t.Error("CollapseBlankLines=false gets overridden to true by default logic")
		}
	})
}

func TestApplyDefaultsWindow(t *testing.T) {
	t.Run("zero config except enabled=false", func(t *testing.T) {
		cfg := Config{}
		applyDefaults(&cfg)

		if cfg.Window.Enabled {
			t.Error("Window.Enabled should default to false")
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
		if cfg.Window.MaxContext != 128000 {
			t.Errorf("MaxContext: got %d", cfg.Window.MaxContext)
		}
	})

	t.Run("sub-fields auto-enable", func(t *testing.T) {
		cfg := Config{
			Window: WindowConfig{Mode: "truncate"},
		}
		applyDefaults(&cfg)

		if !cfg.Window.Enabled {
			t.Error("setting Mode should auto-enable Window")
		}
		if cfg.Window.Mode != "truncate" {
			t.Errorf("Mode preserved: got %q", cfg.Window.Mode)
		}
	})

	t.Run("user values preserved", func(t *testing.T) {
		cfg := Config{
			Window: WindowConfig{
				Enabled:        true,
				ActiveMessages: 10,
				TriggerRatio:   0.5,
			},
		}
		applyDefaults(&cfg)

		if cfg.Window.ActiveMessages != 10 {
			t.Errorf("ActiveMessages preserved: got %d", cfg.Window.ActiveMessages)
		}
		if cfg.Window.TriggerRatio != 0.5 {
			t.Errorf("TriggerRatio preserved: got %f", cfg.Window.TriggerRatio)
		}
	})
}

func TestApplyDefaultsProviders(t *testing.T) {
	t.Run("nil providers gets built-in", func(t *testing.T) {
		cfg := Config{}
		applyDefaults(&cfg)

		if cfg.Providers == nil {
			t.Fatal("Providers should not be nil")
		}
		if _, ok := cfg.Providers["gemini"]; !ok {
			t.Error("built-in gemini provider missing")
		}
		if _, ok := cfg.Providers["deepseek"]; !ok {
			t.Error("built-in deepseek provider missing")
		}
		if _, ok := cfg.Providers["zai"]; !ok {
			t.Error("built-in zai provider missing")
		}
	})

	t.Run("user providers merged with built-in", func(t *testing.T) {
		cfg := Config{
			Providers: map[string]ProviderConfig{
				"custom": {
					URL:           "http://localhost:8080/v1/chat/completions",
					RoutePrefixes: []string{"custom-"},
					AuthStyle:     "bearer",
				},
			},
		}
		applyDefaults(&cfg)

		if _, ok := cfg.Providers["gemini"]; !ok {
			t.Error("built-in gemini should still be present")
		}
		if _, ok := cfg.Providers["custom"]; !ok {
			t.Error("custom provider should be present")
		}
	})

	t.Run("user override wins over built-in", func(t *testing.T) {
		cfg := Config{
			Providers: map[string]ProviderConfig{
				"gemini": {
					URL:           "http://custom-gemini.example.com/v1/chat/completions",
					RoutePrefixes: []string{"my-gemini-"},
					AuthStyle:     "bearer",
				},
			},
		}
		applyDefaults(&cfg)

		gemini := cfg.Providers["gemini"]
		if gemini.URL != "http://custom-gemini.example.com/v1/chat/completions" {
			t.Errorf("gemini URL: got %q", gemini.URL)
		}
		if gemini.AuthStyle != "bearer" {
			t.Errorf("gemini AuthStyle: got %q", gemini.AuthStyle)
		}
	})
}

func TestLoadConfig(t *testing.T) {
	t.Run("valid JSON file", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "config.json")
		content := `{"server":{"listen_addr":":9090","max_body_bytes":5242880}}`
		if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}

		cfg, err := loadConfig(configPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Server.ListenAddr != ":9090" {
			t.Errorf("ListenAddr: got %q", cfg.Server.ListenAddr)
		}
		if cfg.Server.MaxBodyBytes != 5242880 {
			t.Errorf("MaxBodyBytes: got %d", cfg.Server.MaxBodyBytes)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := loadConfig("/nonexistent/path/config.json")
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "config.json")
		if err := os.WriteFile(configPath, []byte(`{invalid json}`), 0644); err != nil {
			t.Fatal(err)
		}

		_, err := loadConfig(configPath)
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})

	t.Run("empty JSON applies all defaults", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "config.json")
		if err := os.WriteFile(configPath, []byte(`{}`), 0644); err != nil {
			t.Fatal(err)
		}

		cfg, err := loadConfig(configPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Server.ListenAddr != ":8080" {
			t.Errorf("ListenAddr default: got %q", cfg.Server.ListenAddr)
		}
		if cfg.SecurityFilter.Engine.Model != "qwen2.5-coder:7b" {
			t.Errorf("SecurityFilter.Engine.Model default: got %q", cfg.SecurityFilter.Engine.Model)
		}
	})

	t.Run("full config roundtrip", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "config.json")
		original := Config{
			Server:     ServerConfig{ListenAddr: ":7070", MaxBodyBytes: 999, TokenRatio: 3.0},
			Governance: GovernanceConfig{ContextSoftLimit: 1000, ContextHardLimit: 5000, RatelimitMaxRPM: 5, RatelimitMaxTPM: 50000},
			SecurityFilter: SecurityFilterConfig{
				Enabled:        true,
				Patterns:       []string{`AKIA[0-9A-Z]{16}`},
				RedactionLabel: "[HIDDEN]",
				Engine:         EngineConfig{URL: "http://localhost:11434/api/generate", Model: "llama3:8b", TimeoutSeconds: 120},
			},
			Window: WindowConfig{Enabled: true, Mode: "truncate", ActiveMessages: 4},
			Agents: map[string]AgentConfig{"test-agent": {Strategy: "fallback"}},
		}
		data, _ := json.Marshal(original)
		if err := os.WriteFile(configPath, data, 0644); err != nil {
			t.Fatal(err)
		}

		cfg, err := loadConfig(configPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Server.ListenAddr != ":7070" {
			t.Errorf("ListenAddr: got %q", cfg.Server.ListenAddr)
		}
		if cfg.Window.Mode != "truncate" {
			t.Errorf("Window.Mode: got %q", cfg.Window.Mode)
		}
		if _, ok := cfg.Agents["test-agent"]; !ok {
			t.Error("Agents should contain test-agent")
		}
	})
}

func TestLoadSecrets(t *testing.T) {
	t.Run("valid secrets file", func(t *testing.T) {
		tmpDir := t.TempDir()
		secretsPath := filepath.Join(tmpDir, "secrets")
		content := `{"client_token":"test-token","provider_keys":{"gemini":"AIza..."}}`
		if err := os.WriteFile(secretsPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CREDENTIALS_DIRECTORY", tmpDir)

		secrets, err := loadSecrets()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if secrets.ClientToken != "test-token" {
			t.Errorf("ClientToken: got %q", secrets.ClientToken)
		}
		if secrets.ProviderKeys["gemini"] != "AIza..." {
			t.Errorf("gemini key: got %q", secrets.ProviderKeys["gemini"])
		}
	})

	t.Run("missing CREDENTIALS_DIRECTORY", func(t *testing.T) {
		t.Setenv("CREDENTIALS_DIRECTORY", "")
		_, err := loadSecrets()
		if err == nil {
			t.Fatal("expected error for missing env var")
		}
	})

	t.Run("missing secrets file", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("CREDENTIALS_DIRECTORY", tmpDir)

		_, err := loadSecrets()
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("missing client_token", func(t *testing.T) {
		tmpDir := t.TempDir()
		secretsPath := filepath.Join(tmpDir, "secrets")
		if err := os.WriteFile(secretsPath, []byte(`{"provider_keys":{}}`), 0644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CREDENTIALS_DIRECTORY", tmpDir)

		_, err := loadSecrets()
		if err == nil {
			t.Fatal("expected error for missing client_token")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		tmpDir := t.TempDir()
		secretsPath := filepath.Join(tmpDir, "secrets")
		if err := os.WriteFile(secretsPath, []byte(`not json`), 0644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CREDENTIALS_DIRECTORY", tmpDir)

		_, err := loadSecrets()
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})

	t.Run("no provider_keys initializes empty map", func(t *testing.T) {
		tmpDir := t.TempDir()
		secretsPath := filepath.Join(tmpDir, "secrets")
		if err := os.WriteFile(secretsPath, []byte(`{"client_token":"tok"}`), 0644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CREDENTIALS_DIRECTORY", tmpDir)

		secrets, err := loadSecrets()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if secrets.ProviderKeys == nil {
			t.Error("ProviderKeys should be initialized to empty map")
		}
	})
}

func TestResolveProviders(t *testing.T) {
	cfg := Config{
		Providers: map[string]ProviderConfig{
			"gemini": {URL: "https://gemini.example.com", RoutePrefixes: []string{"gemini-"}, AuthStyle: "bearer"},
			"openai": {URL: "https://api.openai.com", RoutePrefixes: []string{"gpt-"}, AuthStyle: "bearer"},
		},
	}
	secrets := &SecretsConfig{
		ClientToken:  "tok",
		ProviderKeys: map[string]string{"gemini": "AIza...", "openai": "sk-..."},
	}

	providers := resolveProviders(&cfg, secrets)

	if len(providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(providers))
	}
	if providers["gemini"].APIKey != "AIza..." {
		t.Errorf("gemini APIKey: got %q", providers["gemini"].APIKey)
	}
	if providers["openai"].APIKey != "sk-..." {
		t.Errorf("openai APIKey: got %q", providers["openai"].APIKey)
	}
	if providers["gemini"].Name != "gemini" {
		t.Errorf("gemini Name: got %q", providers["gemini"].Name)
	}

	t.Run("nil secrets gives empty API keys", func(t *testing.T) {
		providers := resolveProviders(&cfg, nil)
		if providers["gemini"].APIKey != "" {
			t.Errorf("expected empty APIKey with nil secrets, got %q", providers["gemini"].APIKey)
		}
	})
}

func TestValidationEndpoints(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected string
	}{
		{
			name:     "gemini",
			url:      "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			expected: "https://generativelanguage.googleapis.com/v1beta/models",
		},
		{
			name:     "deepseek",
			url:      "https://api.deepseek.com/v1/chat/completions",
			expected: "https://api.deepseek.com/models",
		},
		{
			name:     "zai",
			url:      "https://api.z.ai/v1/chat/completions",
			expected: "https://api.z.ai/v1/models",
		},
		{
			name:     "groq",
			url:      "https://api.groq.com/openai/v1/chat/completions",
			expected: "https://api.groq.com/openai/v1/models",
		},
		{
			name:     "together",
			url:      "https://api.together.xyz/v1/chat/completions",
			expected: "https://api.together.xyz/v1/models",
		},
		{
			name:     "openai",
			url:      "https://api.openai.com/v1/chat/completions",
			expected: "https://api.openai.com/v1/models",
		},
		{
			name:     "ollama",
			url:      "http://127.0.0.1:11434/v1/chat/completions",
			expected: "",
		},
		{
			name:     "custom endpoint",
			url:      "https://custom.example.com/v1/chat/completions",
			expected: "https://custom.example.com/v1/models",
		},
		{
			name:     "no chat completions suffix",
			url:      "https://example.com/api",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getValidationEndpoint(tt.url)
			if result != tt.expected {
				t.Errorf("getValidationEndpoint(%q) = %q, want %q", tt.url, result, tt.expected)
			}
		})
	}
}

func TestApplyAuthHeader(t *testing.T) {
	tests := []struct {
		name     string
		provider *Provider
		wantErr  bool
		check    func(t *testing.T, req *http.Request)
	}{
		{
			name: "bearer auth",
			provider: &Provider{
				Name:      "test",
				APIKey:    "test-token",
				AuthStyle: "bearer",
			},
			check: func(t *testing.T, req *http.Request) {
				if req.Header.Get("Authorization") != "Bearer test-token" {
					t.Errorf("Authorization header = %q, want 'Bearer test-token'", req.Header.Get("Authorization"))
				}
			},
		},
		{
			name: "bearer+x-goog auth",
			provider: &Provider{
				Name:      "gemini",
				APIKey:    "AIza...",
				AuthStyle: "bearer+x-goog",
			},
			check: func(t *testing.T, req *http.Request) {
				if req.Header.Get("Authorization") != "Bearer AIza..." {
					t.Errorf("Authorization header = %q, want 'Bearer AIza...'", req.Header.Get("Authorization"))
				}
				if req.Header.Get("x-goog-api-key") != "AIza..." {
					t.Errorf("x-goog-api-key header = %q, want 'AIza...'", req.Header.Get("x-goog-api-key"))
				}
			},
		},
		{
			name: "no auth",
			provider: &Provider{
				Name:      "ollama",
				APIKey:    "",
				AuthStyle: "none",
			},
			check: func(t *testing.T, req *http.Request) {
				if req.Header.Get("Authorization") != "" {
					t.Errorf("Authorization header = %q, want empty", req.Header.Get("Authorization"))
				}
			},
		},
		{
			name: "unsupported auth style",
			provider: &Provider{
				Name:      "bad",
				APIKey:    "key",
				AuthStyle: "custom",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", "https://example.com", nil)
			err := applyAuthHeader(req, tt.provider)
			if (err != nil) != tt.wantErr {
				t.Errorf("applyAuthHeader() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && tt.check != nil {
				tt.check(t, req)
			}
		})
	}
}
