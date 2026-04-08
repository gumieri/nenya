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
	MaxOutput  int    `json:"max_output"`
}

type AgentConfig struct {
	Strategy         string       `json:"strategy"`
	CooldownSeconds  int          `json:"cooldown_seconds"`
	SystemPrompt     string       `json:"system_prompt"`
	SystemPromptFile string       `json:"system_prompt_file"`
	Models           []AgentModel `json:"models"`
}

func (a *AgentConfig) UnmarshalJSON(data []byte) error {
	type alias AgentConfig
	aux := struct {
		Models []json.RawMessage `json:"models"`
		*alias
	}{
		alias: (*alias)(a),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	a.Models = make([]AgentModel, 0, len(aux.Models))
	for _, raw := range aux.Models {
		var m AgentModel
		var modelStr string
		if err := json.Unmarshal(raw, &modelStr); err == nil {
			entry, ok := ModelRegistry[modelStr]
			if !ok {
				return fmt.Errorf("model %q not found in registry", modelStr)
			}
			m = AgentModel{
				Model:      modelStr,
				Provider:   entry.Provider,
				MaxContext: entry.MaxContext,
				MaxOutput:  entry.MaxOutput,
			}
		} else {
			if err := json.Unmarshal(raw, &m); err != nil {
				return fmt.Errorf("invalid model entry: must be a string or an object")
			}
		}
		a.Models = append(a.Models, m)
	}
	return nil
}

type ProviderConfig struct {
	URL                  string   `json:"url"`
	RoutePrefixes        []string `json:"route_prefixes"`
	AuthStyle            string   `json:"auth_style"`
	ApiFormat            string   `json:"api_format"`
	RetryableStatusCodes []int    `json:"retryable_status_codes"`
}

type Provider struct {
	Name                 string
	URL                  string
	APIKey               string
	RoutePrefixes        []string
	AuthStyle            string
	ApiFormat            string
	RetryableStatusCodes []int
}

type Config struct {
	Server         ServerConfig              `json:"server"`
	Governance     GovernanceConfig          `json:"governance"`
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
	UserAgent    string  `json:"user_agent"`
}

type GovernanceConfig struct {
	BlockedExecutionPatterns []string `json:"blocked_execution_patterns"`
	RatelimitMaxRPM          int      `json:"ratelimit_max_rpm"`
	RatelimitMaxTPM          int      `json:"ratelimit_max_tpm"`
	ContextSoftLimit         int      `json:"context_soft_limit"`
	ContextHardLimit         int      `json:"context_hard_limit"`
	TruncationStrategy       string   `json:"truncation_strategy"`
	KeepFirstPercent         float64  `json:"keep_first_percent"`
	KeepLastPercent          float64  `json:"keep_last_percent"`
	RetryableStatusCodes     []int    `json:"retryable_status_codes"`
	rpmSet                   bool     `json:"-"`
	tpmSet                   bool     `json:"-"`
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
	Enabled           bool         `json:"enabled"`
	RedactionLabel    string       `json:"redaction_label"`
	Patterns          []string     `json:"patterns"`
	OutputEnabled     bool         `json:"output_enabled"`
	OutputWindowChars int          `json:"output_window_chars"`
	Engine            EngineConfig `json:"engine"`
	enabledSet        bool         `json:"-"`
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
		if len(s.Patterns) > 0 {
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
	pinSet                bool `json:"-"`
	stableSet             bool `json:"-"`
	skipRedactionSet      bool `json:"-"`
}

func (c *PrefixCacheConfig) UnmarshalJSON(data []byte) error {
	type alias PrefixCacheConfig
	aux := struct {
		PinSystemFirst        *bool `json:"pin_system_first"`
		StableTools           *bool `json:"stable_tools"`
		SkipRedactionOnSystem *bool `json:"skip_redaction_on_system"`
		*alias
	}{
		alias: (*alias)(c),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.PinSystemFirst != nil {
		c.PinSystemFirst = *aux.PinSystemFirst
		c.pinSet = true
	}
	if aux.StableTools != nil {
		c.StableTools = *aux.StableTools
		c.stableSet = true
	}
	if aux.SkipRedactionOnSystem != nil {
		c.SkipRedactionOnSystem = *aux.SkipRedactionOnSystem
		c.skipRedactionSet = true
	}
	return nil
}

type CompactionConfig struct {
	Enabled                bool `json:"enabled"`
	JSONMinify             bool `json:"json_minify"`
	CollapseBlankLines     bool `json:"collapse_blank_lines"`
	TrimTrailingWhitespace bool `json:"trim_trailing_whitespace"`
	NormalizeLineEndings   bool `json:"normalize_line_endings"`
	enabledSet             bool `json:"-"`
	minifySet              bool `json:"-"`
	collapseSet            bool `json:"-"`
	trimSet                bool `json:"-"`
	normalizeSet           bool `json:"-"`
}

func (c *CompactionConfig) UnmarshalJSON(data []byte) error {
	type alias CompactionConfig
	aux := struct {
		Enabled                *bool `json:"enabled"`
		JSONMinify             *bool `json:"json_minify"`
		CollapseBlankLines     *bool `json:"collapse_blank_lines"`
		TrimTrailingWhitespace *bool `json:"trim_trailing_whitespace"`
		NormalizeLineEndings   *bool `json:"normalize_line_endings"`
		*alias
	}{
		alias: (*alias)(c),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.Enabled != nil {
		c.Enabled = *aux.Enabled
		c.enabledSet = true
	}
	if aux.JSONMinify != nil {
		c.JSONMinify = *aux.JSONMinify
		c.minifySet = true
	}
	if aux.CollapseBlankLines != nil {
		c.CollapseBlankLines = *aux.CollapseBlankLines
		c.collapseSet = true
	}
	if aux.TrimTrailingWhitespace != nil {
		c.TrimTrailingWhitespace = *aux.TrimTrailingWhitespace
		c.trimSet = true
	}
	if aux.NormalizeLineEndings != nil {
		c.NormalizeLineEndings = *aux.NormalizeLineEndings
		c.normalizeSet = true
	}
	return nil
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
	providers := make(map[string]ProviderConfig, len(ProviderRegistry))
	for name, entry := range ProviderRegistry {
		providers[name] = entry.ToProviderConfig()
	}
	return providers
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

func applyEngineDefaults(e *EngineConfig) {
	if e.Provider == "" {
		e.Provider = "ollama"
	}
	if e.Model == "" {
		e.Model = "qwen2.5-coder:7b"
	}
	if e.TimeoutSeconds == 0 {
		e.TimeoutSeconds = 600
	}
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
	if cfg.Server.UserAgent == "" {
		cfg.Server.UserAgent = "nenya/1.0"
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
	if len(cfg.Governance.BlockedExecutionPatterns) == 0 {
		cfg.Governance.BlockedExecutionPatterns = []string{
			`(?i)\brm\s+-[a-zA-Z]*[rR][a-zA-Z]*\s+.*(/|\*)`,
			`(?i)\bchmod\s+(?:-R\s+)?777\b`,
			`(?i)\bmkfs\.`,
			`(?i)\bterraform\s+destroy\b`,
			`(?i)\bterragrunt\s+destroy\b`,
			`(?i)\baws\s+s3\s+rb\s+.*--force`,
			`(?i)\baws\s+ec2\s+terminate-instances\b`,
			`(?i)\bkubectl\s+delete\s+(namespace|ns|pv|pvc|crd)\b`,
			`(?i)\bhelm\s+(uninstall|delete)\b`,
			`(?i)\b(DROP|TRUNCATE)\s+(TABLE|DATABASE|SCHEMA)\b`,
			`(?i)\b(shutdown|reboot|poweroff|halt|init\s+0)\b`,
		}
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
			`(?i)-----BEGIN\s+(RSA\s+)?(DSA\s+)?(EC\s+)?PRIVATE\s+KEY\s*-----`,
			`(?i)(aws_access_key_id|aws_secret_access_key)\s*=\s*['"][^'"]{10,}['"]`,
			`(?i)(password|passwd|pwd|secret|token)[\s:=]+['"][^'"]{6,}['"]`,
			`[a-f0-9]{32}:`,
			`(?i)SG\.[a-zA-Z0-9\-_]{22}\.[a-zA-Z0-9\-_]{43}`,
		}
	} else if !cfg.SecurityFilter.enabledSet {
		cfg.SecurityFilter.Enabled = true
	}
	if cfg.SecurityFilter.OutputWindowChars == 0 {
		cfg.SecurityFilter.OutputWindowChars = 4096
	}

	applyEngineDefaults(&cfg.SecurityFilter.Engine)
	applyEngineDefaults(&cfg.Window.Engine)
	if !cfg.PrefixCache.Enabled && (cfg.PrefixCache.PinSystemFirst || cfg.PrefixCache.StableTools || cfg.PrefixCache.SkipRedactionOnSystem) {
		cfg.PrefixCache.Enabled = true
	}
	if !cfg.PrefixCache.PinSystemFirst && !cfg.PrefixCache.pinSet {
		cfg.PrefixCache.PinSystemFirst = true
	}
	if !cfg.PrefixCache.StableTools && !cfg.PrefixCache.stableSet {
		cfg.PrefixCache.StableTools = true
	}
	if !cfg.PrefixCache.SkipRedactionOnSystem && !cfg.PrefixCache.skipRedactionSet {
		cfg.PrefixCache.SkipRedactionOnSystem = true
	}

	if !cfg.Compaction.JSONMinify && !cfg.Compaction.minifySet {
		cfg.Compaction.JSONMinify = true
	}
	if !cfg.Compaction.CollapseBlankLines && !cfg.Compaction.collapseSet {
		cfg.Compaction.CollapseBlankLines = true
	}
	if !cfg.Compaction.TrimTrailingWhitespace && !cfg.Compaction.trimSet {
		cfg.Compaction.TrimTrailingWhitespace = true
	}
	if !cfg.Compaction.NormalizeLineEndings && !cfg.Compaction.normalizeSet {
		cfg.Compaction.NormalizeLineEndings = true
	}

	if !cfg.Compaction.Enabled && !cfg.Compaction.enabledSet && (cfg.Compaction.JSONMinify || cfg.Compaction.CollapseBlankLines || cfg.Compaction.TrimTrailingWhitespace || cfg.Compaction.NormalizeLineEndings) {
		cfg.Compaction.Enabled = true
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
			Name:                 name,
			URL:                  pc.URL,
			APIKey:               apiKey,
			RoutePrefixes:        pc.RoutePrefixes,
			AuthStyle:            pc.AuthStyle,
			ApiFormat:            pc.ApiFormat,
			RetryableStatusCodes: pc.RetryableStatusCodes,
		}
	}
	return providers
}
