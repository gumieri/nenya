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
	Strategy         string       `json:"strategy"`
	CooldownSeconds  int          `json:"cooldown_seconds"`
	SystemPrompt     string       `json:"system_prompt"`
	SystemPromptFile string       `json:"system_prompt_file"`
	Models           []AgentModel `json:"models"`
}

type ProviderConfig struct {
	URL           string   `json:"url"`
	RoutePrefixes []string `json:"route_prefixes"`
	AuthStyle     string   `json:"auth_style"`
	ApiFormat     string   `json:"api_format"`
}

type Provider struct {
	Name          string
	URL           string
	APIKey        string
	RoutePrefixes []string
	AuthStyle     string
	ApiFormat     string
}

type Config struct {
	Server         ServerConfig
	Governance     GovernanceConfig
	SecurityFilter SecurityFilterConfig      `json:"security_filter"`
	PrefixCache    PrefixCacheConfig         `json:"prefix_cache"`
	Compaction     CompactionConfig          `json:"compaction"`
	Window         WindowConfig              `json:"window"`
	Agents         map[string]AgentConfig    `json:"agents"`
	Providers      map[string]ProviderConfig `json:"providers"`
}

type ServerConfig struct {
	ListenAddr   string  `json:"listen_addr"`
	MaxBodyBytes int64   `json:"max_body_bytes"`
	TokenRatio   float64 `json:"token_ratio"`
}

type GovernanceConfig struct {
	RatelimitMaxRPM    int     `json:"ratelimit_max_rpm"`
	RatelimitMaxTPM    int     `json:"ratelimit_max_tpm"`
	ContextSoftLimit   int     `json:"context_soft_limit"`
	ContextHardLimit   int     `json:"context_hard_limit"`
	TruncationStrategy string  `json:"truncation_strategy"`
	KeepFirstPercent   float64 `json:"keep_first_percent"`
	KeepLastPercent    float64 `json:"keep_last_percent"`
	rpmSet             bool    `json:"-"`
	tpmSet             bool    `json:"-"`
}

type SecretsConfig struct {
	ClientToken  string            `json:"client_token"`
	ProviderKeys map[string]string `json:"provider_keys"`
}

type EngineConfig struct {
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	SystemPrompt     string `json:"system_prompt"`
	SystemPromptFile string `json:"system_prompt_file"`
	TimeoutSeconds   int    `json:"timeout_seconds"`
}

type SecurityFilterConfig struct {
	Enabled        bool         `json:"enabled"`
	RedactionLabel string       `json:"redaction_label"`
	Patterns       []string     `json:"patterns"`
	Engine         EngineConfig `json:"engine"`
	enabledSet     bool         `json:"-"`
}

func (s *SecurityFilterConfig) UnmarshalJSON(data []byte) error {
	type alias SecurityFilterConfig // avoid recursion
	aux := struct {
		Enabled *bool `json:"enabled"`
		*alias
	}{
		alias: (*alias)(s),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.Enabled == nil {
		// field not present; default to true if patterns present
		if s.Patterns != nil && len(s.Patterns) > 0 {
			s.Enabled = true
		}
		s.enabledSet = false
	} else {
		s.Enabled = *aux.Enabled
		s.enabledSet = true
	}
	return nil
}

func (g *GovernanceConfig) UnmarshalJSON(data []byte) error {
	type alias GovernanceConfig // avoid recursion
	aux := struct {
		RatelimitMaxRPM *int `json:"ratelimit_max_rpm"`
		RatelimitMaxTPM *int `json:"ratelimit_max_tpm"`
		*alias
	}{
		alias: (*alias)(g),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.RatelimitMaxRPM == nil {
		g.rpmSet = false
	} else {
		g.rpmSet = true
		g.RatelimitMaxRPM = *aux.RatelimitMaxRPM
	}
	if aux.RatelimitMaxTPM == nil {
		g.tpmSet = false
	} else {
		g.tpmSet = true
		g.RatelimitMaxTPM = *aux.RatelimitMaxTPM
	}
	return nil
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
	Enabled         bool         `json:"enabled"`
	Mode            string       `json:"mode"`
	ActiveMessages  int          `json:"active_messages"`
	TriggerRatio    float64      `json:"trigger_ratio"`
	SummaryMaxRunes int          `json:"summary_max_runes"`
	MaxContext      int          `json:"max_context"`
	Engine          EngineConfig `json:"engine"`
}

func builtInProviders() map[string]ProviderConfig {
	return map[string]ProviderConfig{
		"gemini": {
			URL:           "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			RoutePrefixes: []string{"gemini-"},
			AuthStyle:     "bearer+x-goog",
		},
		"deepseek": {
			URL:           "https://api.deepseek.com/chat/completions",
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
	data = stripComments(data)
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %v", filename, err)
	}
	applyDefaults(&cfg)
	return &cfg, nil
}

func stripComments(data []byte) []byte {
	var result []byte
	i := 0
	n := len(data)
	inString := false
	for i < n {
		if !inString && i+1 < n && data[i] == '/' && data[i+1] == '/' {
			// single-line comment: skip until newline
			for i < n && data[i] != '\n' {
				i++
			}
			continue
		}
		if !inString && i+1 < n && data[i] == '/' && data[i+1] == '*' {
			// multi-line comment: skip until */
			for i < n && !(data[i] == '*' && i+1 < n && data[i+1] == '/') {
				i++
			}
			if i+1 < n {
				i += 2 // skip */
			}
			continue
		}
		if data[i] == '"' {
			// count preceding backslashes to determine if quote is escaped
			backslashCount := 0
			for j := i - 1; j >= 0 && data[j] == '\\'; j-- {
				backslashCount++
			}
			if backslashCount%2 == 0 {
				// not escaped
				inString = !inString
			}
		}
		result = append(result, data[i])
		i++
	}
	return result
}

func loadPromptFile(filePath string, directPrompt string, defaultPrompt string) (string, error) {
	// Priority: 1. direct prompt, 2. file, 3. default
	if directPrompt != "" {
		return directPrompt, nil
	}
	if filePath == "" {
		return defaultPrompt, nil
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read prompt file %s: %v", filePath, err)
	}
	return string(data), nil
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
	if !cfg.Governance.tpmSet && cfg.Governance.RatelimitMaxTPM == 0 {
		cfg.Governance.RatelimitMaxTPM = 250000
	}
	if !cfg.Governance.rpmSet && cfg.Governance.RatelimitMaxRPM == 0 {
		cfg.Governance.RatelimitMaxRPM = 15
	}
	if cfg.Governance.ContextSoftLimit == 0 {
		cfg.Governance.ContextSoftLimit = 4000
	}
	if cfg.Governance.ContextHardLimit == 0 {
		cfg.Governance.ContextHardLimit = 24000
	}
	if cfg.Governance.TruncationStrategy == "" {
		cfg.Governance.TruncationStrategy = "middle-out"
	}
	if cfg.Governance.KeepFirstPercent == 0 {
		cfg.Governance.KeepFirstPercent = 15.0
	}
	if cfg.Governance.KeepLastPercent == 0 {
		cfg.Governance.KeepLastPercent = 25.0
	}
	if cfg.SecurityFilter.RedactionLabel == "" {
		cfg.SecurityFilter.RedactionLabel = "[REDACTED]"
	}
	if cfg.SecurityFilter.Patterns == nil {
		if !cfg.SecurityFilter.enabledSet {
			cfg.SecurityFilter.Enabled = true
		}
		cfg.SecurityFilter.Patterns = []string{
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
	} else if !cfg.SecurityFilter.enabledSet {
		cfg.SecurityFilter.Enabled = true
	}

	if cfg.SecurityFilter.Engine.Provider == "" {
		cfg.SecurityFilter.Engine.Provider = "ollama"
	}
	if cfg.SecurityFilter.Engine.Model == "" {
		cfg.SecurityFilter.Engine.Model = "qwen2.5-coder:7b"
	}
	if cfg.SecurityFilter.Engine.TimeoutSeconds == 0 {
		cfg.SecurityFilter.Engine.TimeoutSeconds = 600
	}
	if cfg.Window.Engine.Provider == "" {
		cfg.Window.Engine.Provider = "ollama"
	}
	if cfg.Window.Engine.Model == "" {
		cfg.Window.Engine.Model = "qwen2.5-coder:7b"
	}
	if cfg.Window.Engine.TimeoutSeconds == 0 {
		cfg.Window.Engine.TimeoutSeconds = 600
	}
	if !cfg.PrefixCache.Enabled && (cfg.PrefixCache.PinSystemFirst || cfg.PrefixCache.StableTools || cfg.PrefixCache.SkipRedactionOnSystem) {
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

	if cfg.Providers == nil {
		cfg.Providers = make(map[string]ProviderConfig)
	}
	for name, builtIn := range builtInProviders() {
		if _, exists := cfg.Providers[name]; !exists {
			cfg.Providers[name] = builtIn
		}
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
			ApiFormat:     pc.ApiFormat,
		}
	}
	return providers
}
