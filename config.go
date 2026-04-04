package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type AgentModel struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	URL        string `json:"url"`
	MaxContext int    `json:"max_context"`
}

type AgentConfig struct {
	Strategy        string       `json:"strategy"`
	CooldownSeconds int          `json:"cooldown_seconds"`
	Models          []AgentModel `json:"models"`
}

type ProviderConfig struct {
	URL           string   `json:"url"`
	RoutePrefixes []string `json:"route_prefixes"`
	AuthStyle     string   `json:"auth_style"`
}

type Provider struct {
	Name          string
	URL           string
	APIKey        string
	RoutePrefixes []string
	AuthStyle     string
}

type Config struct {
	Server      ServerConfig
	Interceptor InterceptorConfig
	Ollama      OllamaConfig
	RateLimit   RateLimitConfig `json:"ratelimit"`
	Filter      FilterConfig
	PrefixCache PrefixCacheConfig         `json:"prefix_cache"`
	Compaction  CompactionConfig          `json:"compaction"`
	Window      WindowConfig              `json:"window"`
	Agents      map[string]AgentConfig    `json:"agents"`
	Providers   map[string]ProviderConfig `json:"providers"`
}

type ServerConfig struct {
	ListenAddr   string  `json:"listen_addr"`
	MaxBodyBytes int64   `json:"max_body_bytes"`
	TokenRatio   float64 `json:"token_ratio"`
}

type InterceptorConfig struct {
	SoftLimit          int     `json:"soft_limit"`
	HardLimit          int     `json:"hard_limit"`
	TruncationStrategy string  `json:"truncation_strategy"`
	KeepFirstPercent   float64 `json:"keep_first_percent"`
	KeepLastPercent    float64 `json:"keep_last_percent"`
}

type RateLimitConfig struct {
	MaxTPM int `json:"max_tpm"`
	MaxRPM int `json:"max_rpm"`
}

type SecretsConfig struct {
	ClientToken  string            `json:"client_token"`
	ProviderKeys map[string]string `json:"provider_keys"`
}

type OllamaConfig struct {
	URL            string `json:"url"`
	Model          string `json:"model"`
	SystemPrompt   string `json:"system_prompt"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type FilterConfig struct {
	Enabled        bool     `json:"enabled"`
	Patterns       []string `json:"patterns"`
	RedactionLabel string   `json:"redaction_label"`
}

type PrefixCacheConfig struct {
	Enabled               bool `json:"enabled"`
	PinSystemFirst        bool `json:"pin_system_first"`
	StableTools           bool `json:"stable_tools"`
	SkipRedactionOnSystem bool `json:"skip_redaction_on_system"`
}

type CompactionConfig struct {
	Enabled                bool `json:"enabled"`
	JSONMinify             bool `json:"json_minify"`
	CollapseBlankLines     bool `json:"collapse_blank_lines"`
	TrimTrailingWhitespace bool `json:"trim_trailing_whitespace"`
	NormalizeLineEndings   bool `json:"normalize_line_endings"`
}

type WindowConfig struct {
	Enabled         bool    `json:"enabled"`
	Mode            string  `json:"mode"`
	ActiveMessages  int     `json:"active_messages"`
	TriggerRatio    float64 `json:"trigger_ratio"`
	SummaryMaxRunes int     `json:"summary_max_runes"`
	MaxContext      int     `json:"max_context"`
}

func builtInProviders() map[string]ProviderConfig {
	return map[string]ProviderConfig{
		"gemini": {
			URL:           "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			RoutePrefixes: []string{"gemini-"},
			AuthStyle:     "bearer+x-goog",
		},
		"deepseek": {
			URL:           "https://api.deepseek.com/v1/chat/completions",
			RoutePrefixes: []string{"deepseek-"},
			AuthStyle:     "bearer",
		},
		"zai": {
			URL:           "https://api.z.ai/v1/chat/completions",
			RoutePrefixes: []string{"zai-", "glm-"},
			AuthStyle:     "bearer",
		},
		"groq": {
			URL:           "https://api.groq.com/openai/v1/chat/completions",
			RoutePrefixes: []string{"llama-", "llama3-", "mixtral-", "whisper-"},
			AuthStyle:     "bearer",
		},
		"together": {
			URL:           "https://api.together.xyz/v1/chat/completions",
			RoutePrefixes: []string{"meta-llama/", "mistralai/", "qwen/", "together/"},
			AuthStyle:     "bearer",
		},
		"ollama": {
			URL:           "http://127.0.0.1:11434/v1/chat/completions",
			RoutePrefixes: nil,
			AuthStyle:     "none",
		},
	}
}

func loadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %v", filename, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %v", filename, err)
	}

	applyDefaults(&cfg)

	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Server.ListenAddr == "" {
		cfg.Server.ListenAddr = ":8080"
	}
	if cfg.Server.MaxBodyBytes == 0 {
		cfg.Server.MaxBodyBytes = 10 << 20
	}
	if cfg.Server.TokenRatio == 0 {
		cfg.Server.TokenRatio = 4.0
	}
	if cfg.Interceptor.SoftLimit == 0 {
		cfg.Interceptor.SoftLimit = 4000
	}
	if cfg.Interceptor.HardLimit == 0 {
		cfg.Interceptor.HardLimit = 24000
	}
	if cfg.Interceptor.TruncationStrategy == "" {
		cfg.Interceptor.TruncationStrategy = "middle-out"
	}
	if cfg.Interceptor.KeepFirstPercent == 0 {
		cfg.Interceptor.KeepFirstPercent = 15.0
	}
	if cfg.Interceptor.KeepLastPercent == 0 {
		cfg.Interceptor.KeepLastPercent = 25.0
	}
	if cfg.Ollama.URL == "" {
		cfg.Ollama.URL = "http://127.0.0.1:11434/api/generate"
	}
	if cfg.Ollama.Model == "" {
		cfg.Ollama.Model = "qwen2.5-coder:7b"
	}
	if cfg.Ollama.SystemPrompt == "" {
		cfg.Ollama.SystemPrompt = "You are a data privacy filter. Summarize the following log/code error in 5 lines. REMOVE any IP addresses, AWS keys (AKIA...), or passwords. Keep only the technical core of the error."
	}
	if cfg.Ollama.TimeoutSeconds == 0 {
		cfg.Ollama.TimeoutSeconds = 600
	}

	if cfg.Filter.RedactionLabel == "" {
		cfg.Filter.RedactionLabel = "[REDACTED]"
	}
	if cfg.Filter.Patterns == nil {
		cfg.Filter.Enabled = true
		cfg.Filter.Patterns = []string{
			`(?i)AKIA[0-9A-Z]{16}`,
			`(?i)gh(p|o|s)_[a-zA-Z0-9]{36,255}`,
			`(?i)ya29\.[0-9A-Za-z\-_]+`,
			`(?i)sk-[a-zA-Z0-9]{48}`,
			`(?i)-----BEGIN (RSA|DSA|EC|PRIVATE) KEY-----`,
			`(?i)(aws_access_key_id|aws_secret_access_key)\s*=\s*['"][^'"]{10,}['"]`,
			`(?i)(password|passwd|pwd|secret|token|key)[\s:=]+['"][^'"]{6,}['"]`,
			`[a-f0-9]{32}:`,
			`(?i)SG\.[a-zA-Z0-9\-_]{22}\.[a-zA-Z0-9\-_]{43}`,
		}
	}

	if cfg.Providers == nil {
		cfg.Providers = make(map[string]ProviderConfig)
	}
	for name, builtIn := range builtInProviders() {
		if _, exists := cfg.Providers[name]; !exists {
			cfg.Providers[name] = builtIn
		}
	}

	if !cfg.PrefixCache.Enabled && cfg.PrefixCache.PinSystemFirst || cfg.PrefixCache.StableTools || cfg.PrefixCache.SkipRedactionOnSystem {
		cfg.PrefixCache.Enabled = true
	}
	if !cfg.PrefixCache.PinSystemFirst {
		cfg.PrefixCache.PinSystemFirst = true
	}
	if !cfg.PrefixCache.StableTools {
		cfg.PrefixCache.StableTools = true
	}
	if !cfg.PrefixCache.SkipRedactionOnSystem {
		cfg.PrefixCache.SkipRedactionOnSystem = true
	}

	if !cfg.Compaction.Enabled && (cfg.Compaction.JSONMinify || cfg.Compaction.CollapseBlankLines || cfg.Compaction.TrimTrailingWhitespace || cfg.Compaction.NormalizeLineEndings) {
		cfg.Compaction.Enabled = true
	}
	if !cfg.Compaction.JSONMinify {
		cfg.Compaction.JSONMinify = true
	}
	if !cfg.Compaction.CollapseBlankLines {
		cfg.Compaction.CollapseBlankLines = true
	}
	if !cfg.Compaction.TrimTrailingWhitespace {
		cfg.Compaction.TrimTrailingWhitespace = true
	}
	if !cfg.Compaction.NormalizeLineEndings {
		cfg.Compaction.NormalizeLineEndings = true
	}

	if !cfg.Window.Enabled && (cfg.Window.Mode != "" || cfg.Window.ActiveMessages != 0 || cfg.Window.TriggerRatio != 0 || cfg.Window.SummaryMaxRunes != 0 || cfg.Window.MaxContext != 0) {
		cfg.Window.Enabled = true
	}
	if cfg.Window.Mode == "" {
		cfg.Window.Mode = "summarize"
	}
	if cfg.Window.ActiveMessages == 0 {
		cfg.Window.ActiveMessages = 6
	}
	if cfg.Window.TriggerRatio == 0 {
		cfg.Window.TriggerRatio = 0.8
	}
	if cfg.Window.SummaryMaxRunes == 0 {
		cfg.Window.SummaryMaxRunes = 4000
	}
	if cfg.Window.MaxContext == 0 {
		cfg.Window.MaxContext = 128000
	}
}

func loadSecrets() (*SecretsConfig, error) {
	credDir := os.Getenv("CREDENTIALS_DIRECTORY")
	if credDir == "" {
		return nil, fmt.Errorf("CREDENTIALS_DIRECTORY not set")
	}
	secretsPath := filepath.Join(credDir, "secrets")
	data, err := os.ReadFile(secretsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read secrets file %s: %v", secretsPath, err)
	}

	var secrets SecretsConfig
	if err := json.Unmarshal(data, &secrets); err != nil {
		return nil, fmt.Errorf("failed to parse secrets JSON: %v", err)
	}

	if secrets.ClientToken == "" {
		return nil, fmt.Errorf("client_token missing in secrets")
	}
	if secrets.ProviderKeys == nil {
		secrets.ProviderKeys = make(map[string]string)
	}

	return &secrets, nil
}

func resolveProviders(cfg *Config, secrets *SecretsConfig) map[string]*Provider {
	providers := make(map[string]*Provider, len(cfg.Providers))
	for name, pc := range cfg.Providers {
		apiKey := ""
		if secrets != nil {
			apiKey = secrets.ProviderKeys[name]
		}
		providers[name] = &Provider{
			Name:          name,
			URL:           pc.URL,
			APIKey:        apiKey,
			RoutePrefixes: pc.RoutePrefixes,
			AuthStyle:     pc.AuthStyle,
		}
	}
	return providers
}
