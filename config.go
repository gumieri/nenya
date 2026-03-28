package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	toml "github.com/pelletier/go-toml/v2"
)

const (
	defaultGeminiURL        = "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
	defaultDeepSeekURL      = "https://api.deepseek.com/v1/chat/completions"
	defaultZaiURL           = "https://api.z.ai/v1/chat/completions"
	defaultGroqURL          = "https://api.groq.com/openai/v1/chat/completions"
	defaultTogetherURL      = "https://api.together.xyz/v1/chat/completions"
	defaultOllamaOpenAIURL  = "http://127.0.0.1:11434/v1/chat/completions"
	defaultAgentCooldownSec = 60
)

// AgentModel is a single provider+model entry in an agent's fallback chain.
type AgentModel struct {
	Provider   string `toml:"provider"`    // gemini | deepseek | zai | ollama
	Model      string `toml:"model"`       // model ID sent to the provider
	URL        string `toml:"url"`         // optional URL override for this entry
	MaxContext int    `toml:"max_context"` // skip if request tokens > this (0 = no limit)
}

// AgentConfig defines a named agent with an ordered fallback chain of models.
type AgentConfig struct {
	Strategy        string       `toml:"strategy"`         // "round-robin" (default) | "fallback"
	CooldownSeconds int          `toml:"cooldown_seconds"` // 0 → defaultAgentCooldownSec
	Models          []AgentModel `toml:"models"`
}

// Config holds the environment and core configurations.
type Config struct {
	Server      ServerConfig
	Interceptor InterceptorConfig
	Ollama      OllamaConfig
	RateLimit   RateLimitConfig        `toml:"ratelimit"`
	Upstream    UpstreamConfig
	Filter      FilterConfig
	Agents      map[string]AgentConfig `toml:"agents"`
}

type ServerConfig struct {
	ListenAddr   string `toml:"listen_addr"`
	MaxBodyBytes int64  `toml:"max_body_bytes"`
}

type InterceptorConfig struct {
	SoftLimit          int     `toml:"soft_limit"`          // characters (runes)
	HardLimit          int     `toml:"hard_limit"`          // characters (runes)
	TruncationStrategy string  `toml:"truncation_strategy"` // "middle-out"
	KeepFirstPercent   float64 `toml:"keep_first_percent"`  // e.g., 15.0
	KeepLastPercent    float64 `toml:"keep_last_percent"`   // e.g., 25.0
}

type RateLimitConfig struct {
	MaxTPM int `toml:"max_tpm"` // tokens per minute
	MaxRPM int `toml:"max_rpm"` // requests per minute
}

type UpstreamConfig struct {
	GeminiURL   string `toml:"gemini_url"`
	DeepSeekURL string `toml:"deepseek_url"`
	ZaiURL      string `toml:"zai_url"`
	GroqURL     string `toml:"groq_url"`
	TogetherURL string `toml:"together_url"`
}

type SecretsConfig struct {
	ClientToken string `json:"client_token"` // auth for clients
	GeminiKey   string `json:"gemini_key"`   // Gemini API key
	DeepSeekKey string `json:"deepseek_key"` // DeepSeek API key
	ZaiKey      string `json:"zai_key"`      // z.ai API key
	GroqKey     string `json:"groq_key"`     // Groq API key
	TogetherKey string `json:"together_key"` // Together AI API key
}

type OllamaConfig struct {
	URL            string `toml:"url"`
	Model          string `toml:"model"`
	SystemPrompt   string `toml:"system_prompt"`
	TimeoutSeconds int    `toml:"timeout_seconds"` // timeout for local LLM inference
}

type FilterConfig struct {
	Enabled        bool     `toml:"enabled"`
	Patterns       []string `toml:"patterns"`
	RedactionLabel string   `toml:"redaction_label"`
}

// loadConfig reads and parses a TOML configuration file.
func loadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %v", filename, err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %v", filename, err)
	}

	// Apply defaults for fields not set in the config file.
	if cfg.Server.ListenAddr == "" {
		cfg.Server.ListenAddr = ":8080"
	}
	if cfg.Server.MaxBodyBytes == 0 {
		cfg.Server.MaxBodyBytes = 10 << 20 // 10 MB default
	}
	if cfg.Interceptor.SoftLimit == 0 {
		cfg.Interceptor.SoftLimit = 4000 // characters
	}
	if cfg.Interceptor.HardLimit == 0 {
		cfg.Interceptor.HardLimit = 24000 // characters
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
		cfg.Ollama.TimeoutSeconds = 600 // 10 minutes — generous default for local inference
	}

	// Default filter settings.
	// Because bool's zero value is false, we cannot distinguish "user wrote
	// enabled = false" from "user omitted the field". We key the default
	// behaviour on Patterns: if the user did not configure any patterns the
	// filter defaults to enabled with built-in patterns. To opt out, set
	// patterns = [] in the config — an explicit empty list disables the filter
	// regardless of the enabled field.
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
			`(?i)\b[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\b`,
			`(?i)[a-f0-9]{32}:`,
			`(?i)SG\.[a-zA-Z0-9\-_]{22}\.[a-zA-Z0-9\-_]{43}`,
		}
	}

	// Default upstream URLs
	if cfg.Upstream.GeminiURL == "" {
		cfg.Upstream.GeminiURL = defaultGeminiURL
	}
	if cfg.Upstream.DeepSeekURL == "" {
		cfg.Upstream.DeepSeekURL = defaultDeepSeekURL
	}
	if cfg.Upstream.ZaiURL == "" {
		cfg.Upstream.ZaiURL = defaultZaiURL
	}
	if cfg.Upstream.GroqURL == "" {
		cfg.Upstream.GroqURL = defaultGroqURL
	}
	if cfg.Upstream.TogetherURL == "" {
		cfg.Upstream.TogetherURL = defaultTogetherURL
	}

	return &cfg, nil
}

// loadSecrets reads the JSON secrets file from systemd credentials.
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

	// Validate required fields
	if secrets.ClientToken == "" {
		return nil, fmt.Errorf("client_token missing in secrets")
	}
	// API keys can be empty if not using that upstream
	// but warn if they might be needed

	return &secrets, nil
}
