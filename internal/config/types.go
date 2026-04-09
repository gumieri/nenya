package config

import (
	"encoding/json"
	"fmt"
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

func (g *GovernanceConfig) RPMSet() bool { return g.rpmSet }
func (g *GovernanceConfig) TPMSet() bool { return g.tpmSet }

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

func (s *SecurityFilterConfig) EnabledWasSet() bool { return s.enabledSet }

func (s *SecurityFilterConfig) UnmarshalJSON(data []byte) error {
	type alias SecurityFilterConfig
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
	type alias GovernanceConfig
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

func (c *PrefixCacheConfig) PinWasSet() bool           { return c.pinSet }
func (c *PrefixCacheConfig) StableWasSet() bool        { return c.stableSet }
func (c *PrefixCacheConfig) SkipRedactionWasSet() bool { return c.skipRedactionSet }

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

func (c *CompactionConfig) EnabledWasSet() bool  { return c.enabledSet }
func (c *CompactionConfig) MinifyWasSet() bool   { return c.minifySet }
func (c *CompactionConfig) CollapseWasSet() bool { return c.collapseSet }
func (c *CompactionConfig) TrimWasSet() bool     { return c.trimSet }
func (c *CompactionConfig) NormWasSet() bool     { return c.normalizeSet }

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
