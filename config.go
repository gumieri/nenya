package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	toml "github.com/pelletier/go-toml/v2"
)

type AgentModel struct {
	Provider   string `toml:"provider"`
	Model      string `toml:"model"`
	URL        string `toml:"url"`
	MaxContext int    `toml:"max_context"`
}

type AgentConfig struct {
	Strategy        string       `toml:"strategy"`
	CooldownSeconds int          `toml:"cooldown_seconds"`
	Models          []AgentModel `toml:"models"`
}

type ProviderConfig struct {
	URL           string   `toml:"url"`
	RoutePrefixes []string `toml:"route_prefixes"`
	AuthStyle     string   `toml:"auth_style"`
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
	RateLimit   RateLimitConfig `toml:"ratelimit"`
	Filter      FilterConfig
	Agents      map[string]AgentConfig    `toml:"agents"`
	Providers   map[string]ProviderConfig `toml:"providers"`
}

type ServerConfig struct {
	ListenAddr   string `toml:"listen_addr"`
	MaxBodyBytes int64  `toml:"max_body_bytes"`
}

type InterceptorConfig struct {
	SoftLimit          int     `toml:"soft_limit"`
	HardLimit          int     `toml:"hard_limit"`
	TruncationStrategy string  `toml:"truncation_strategy"`
	KeepFirstPercent   float64 `toml:"keep_first_percent"`
	KeepLastPercent    float64 `toml:"keep_last_percent"`
}

type RateLimitConfig struct {
	MaxTPM int `toml:"max_tpm"`
	MaxRPM int `toml:"max_rpm"`
}

type SecretsConfig struct {
	ClientToken  string            `json:"client_token"`
	ProviderKeys map[string]string `json:"provider_keys"`
}

type OllamaConfig struct {
	URL            string `toml:"url"`
	Model          string `toml:"model"`
	SystemPrompt   string `toml:"system_prompt"`
	TimeoutSeconds int    `toml:"timeout_seconds"`
}

type FilterConfig struct {
	Enabled        bool     `toml:"enabled"`
	Patterns       []string `toml:"patterns"`
	RedactionLabel string   `toml:"redaction_label"`
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
	if err := toml.Unmarshal(data, &cfg); err != nil {
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
