package main

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
			cfg := tt.before
			applyDefaults(&cfg)
			tt.check(t, &cfg)
		})
	}
}

func TestApplyDefaultsInterceptor(t *testing.T) {
	cfg := Config{}
	applyDefaults(&cfg)

	if cfg.Interceptor.SoftLimit != 4000 {
		t.Errorf("SoftLimit: got %d", cfg.Interceptor.SoftLimit)
	}
	if cfg.Interceptor.HardLimit != 24000 {
		t.Errorf("HardLimit: got %d", cfg.Interceptor.HardLimit)
	}
	if cfg.Interceptor.TruncationStrategy != "middle-out" {
		t.Errorf("TruncationStrategy: got %q", cfg.Interceptor.TruncationStrategy)
	}
	if cfg.Interceptor.KeepFirstPercent != 15.0 {
		t.Errorf("KeepFirstPercent: got %f", cfg.Interceptor.KeepFirstPercent)
	}
	if cfg.Interceptor.KeepLastPercent != 25.0 {
		t.Errorf("KeepLastPercent: got %f", cfg.Interceptor.KeepLastPercent)
	}

	cfg2 := Config{
		Interceptor: InterceptorConfig{
			SoftLimit:          8000,
			HardLimit:          48000,
			TruncationStrategy: "head-tail",
			KeepFirstPercent:   30.0,
			KeepLastPercent:    10.0,
		},
	}
	applyDefaults(&cfg2)
	if cfg2.Interceptor.SoftLimit != 8000 {
		t.Errorf("SoftLimit preserved: got %d", cfg2.Interceptor.SoftLimit)
	}
	if cfg2.Interceptor.HardLimit != 48000 {
		t.Errorf("HardLimit preserved: got %d", cfg2.Interceptor.HardLimit)
	}
	if cfg2.Interceptor.TruncationStrategy != "head-tail" {
		t.Errorf("TruncationStrategy preserved: got %q", cfg2.Interceptor.TruncationStrategy)
	}
}

func TestApplyDefaultsOllama(t *testing.T) {
	cfg := Config{}
	applyDefaults(&cfg)

	if cfg.Ollama.URL != "http://127.0.0.1:11434/api/generate" {
		t.Errorf("URL: got %q", cfg.Ollama.URL)
	}
	if cfg.Ollama.Model != "qwen2.5-coder:7b" {
		t.Errorf("Model: got %q", cfg.Ollama.Model)
	}
	if cfg.Ollama.TimeoutSeconds != 600 {
		t.Errorf("TimeoutSeconds: got %d", cfg.Ollama.TimeoutSeconds)
	}
	if cfg.Ollama.SystemPrompt == "" {
		t.Error("SystemPrompt: expected non-empty default")
	}

	cfg2 := Config{
		Ollama: OllamaConfig{
			URL:            "http://localhost:11434/api/generate",
			Model:          "llama3:8b",
			TimeoutSeconds: 120,
		},
	}
	applyDefaults(&cfg2)
	if cfg2.Ollama.URL != "http://localhost:11434/api/generate" {
		t.Errorf("URL preserved: got %q", cfg2.Ollama.URL)
	}
	if cfg2.Ollama.Model != "llama3:8b" {
		t.Errorf("Model preserved: got %q", cfg2.Ollama.Model)
	}
	if cfg2.Ollama.TimeoutSeconds != 120 {
		t.Errorf("TimeoutSeconds preserved: got %d", cfg2.Ollama.TimeoutSeconds)
	}
}

func TestApplyDefaultsFilter(t *testing.T) {
	t.Run("empty config gets built-in patterns", func(t *testing.T) {
		cfg := Config{}
		applyDefaults(&cfg)

		if !cfg.Filter.Enabled {
			t.Error("Filter.Enabled should default to true")
		}
		if cfg.Filter.RedactionLabel != "[REDACTED]" {
			t.Errorf("RedactionLabel: got %q", cfg.Filter.RedactionLabel)
		}
		if len(cfg.Filter.Patterns) == 0 {
			t.Error("Patterns: expected non-empty built-in patterns")
		}
	})

	t.Run("custom patterns replace built-in", func(t *testing.T) {
		cfg := Config{
			Filter: FilterConfig{
				Patterns: []string{`custom-pattern`},
			},
		}
		applyDefaults(&cfg)

		if len(cfg.Filter.Patterns) != 1 {
			t.Errorf("Patterns: expected 1 custom, got %d", len(cfg.Filter.Patterns))
		}
		if cfg.Filter.Patterns[0] != `custom-pattern` {
			t.Errorf("Patterns[0]: got %q", cfg.Filter.Patterns[0])
		}
	})

	t.Run("custom redaction label preserved", func(t *testing.T) {
		cfg := Config{
			Filter: FilterConfig{
				RedactionLabel: "[SECRET]",
			},
		}
		applyDefaults(&cfg)

		if cfg.Filter.RedactionLabel != "[SECRET]" {
			t.Errorf("RedactionLabel: got %q", cfg.Filter.RedactionLabel)
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
		if cfg.Ollama.Model != "qwen2.5-coder:7b" {
			t.Errorf("Ollama.Model default: got %q", cfg.Ollama.Model)
		}
	})

	t.Run("full config roundtrip", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "config.json")
		original := Config{
			Server:      ServerConfig{ListenAddr: ":7070", MaxBodyBytes: 999, TokenRatio: 3.0},
			Interceptor: InterceptorConfig{SoftLimit: 1000, HardLimit: 5000},
			Ollama:      OllamaConfig{URL: "http://localhost:11434/api/generate", Model: "llama3:8b", TimeoutSeconds: 120},
			RateLimit:   RateLimitConfig{MaxRPM: 5, MaxTPM: 50000},
			Filter:      FilterConfig{Enabled: true, Patterns: []string{`AKIA[0-9A-Z]{16}`}, RedactionLabel: "[HIDDEN]"},
			Window:      WindowConfig{Enabled: true, Mode: "truncate", ActiveMessages: 4},
			Agents:      map[string]AgentConfig{"test-agent": {Strategy: "fallback"}},
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
