package config

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// AgentModel defines a single model entry within an agent's model list,
// specifying the provider, model name, URL override, and context limits.
// Supports static entries (provider/model) and dynamic entries (provider_rgx/model_rgx)
// that expand against the discovery catalog at runtime.
type AgentModel struct {
	Provider             string   `json:"provider"`
	Model                string   `json:"model"`
	URL                  string   `json:"url"`
	MaxContext           int      `json:"max_context"`
	MaxOutput            int      `json:"max_output"`
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`
	ProviderRgx          string   `json:"provider_rgx,omitempty"` // regex matching provider name from discovery
	ModelRgx             string   `json:"model_rgx,omitempty"`    // regex matching model name from discovery

	providerRE *regexp.Regexp // compiled regex for provider matching
	modelRE    *regexp.Regexp // compiled regex for model matching
}

// CompileRegex compiles the regex patterns in the model entry.
// Returns an error if any pattern is invalid.
func (m *AgentModel) CompileRegex() error {
	if m.ProviderRgx != "" {
		re, err := regexp.Compile(m.ProviderRgx)
		if err != nil {
			return fmt.Errorf("invalid provider_rgx %q: %w", m.ProviderRgx, err)
		}
		m.providerRE = re
	}
	if m.ModelRgx != "" {
		re, err := regexp.Compile(m.ModelRgx)
		if err != nil {
			return fmt.Errorf("invalid model_rgx %q: %w", m.ModelRgx, err)
		}
		m.modelRE = re
	}
	return nil
}

// MatchesCatalog returns true if the model entry matches the given provider and model
// from the discovery catalog using the compiled regex patterns.
func (m *AgentModel) MatchesCatalog(provider, model string) bool {
	if m.providerRE != nil && !m.providerRE.MatchString(provider) {
		return false
	}
	if m.modelRE != nil && !m.modelRE.MatchString(model) {
		return false
	}
	return true
}

// IsDynamic returns true if the model entry has regex patterns and should be
// expanded against the discovery catalog at runtime.
func (m *AgentModel) IsDynamic() bool {
	return m.providerRE != nil || m.modelRE != nil
}

// AgentConfig defines an agent (a named alias for one or more models)
// with routing strategy, cooldown, retry, and MCP configuration.
type AgentConfig struct {
	Strategy          string          `json:"strategy"`
	CooldownSeconds   int             `json:"cooldown_seconds"`
	FailureThreshold  int             `json:"failure_threshold"`
	FailureWindowSec  int             `json:"failure_window_secs"`
	SuccessThreshold  int             `json:"success_threshold"`
	MaxRetries        int             `json:"max_retries"`
	SystemPrompt      string          `json:"system_prompt"`
	SystemPromptFile  string          `json:"system_prompt_file"`
	ForceSystemPrompt bool            `json:"force_system_prompt"`
	Models            []AgentModel    `json:"models,omitempty"`
	MCP               *AgentMCPConfig `json:"mcp,omitempty"`
	BudgetLimitUSD    float64         `json:"budget_limit_usd,omitempty"`
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
	URL                  string          `json:"url"`
	RoutePrefixes        []string        `json:"route_prefixes"`
	AuthStyle            string          `json:"auth_style"`
	ApiFormat            string          `json:"api_format"`
	TimeoutSeconds       int             `json:"timeout_seconds"`
	RetryableStatusCodes []int           `json:"retryable_status_codes"`
	Thinking             *ThinkingConfig `json:"thinking,omitempty"`
}

// ThinkingConfig controls thinking/reasoning mode activation for a provider.
// When nil (omitted in config), auto mode is used: thinking is enabled when
// the model's capabilities indicate it supports reasoning.
// When set, the Enabled field takes precedence over model-level detection.
type ThinkingConfig struct {
	Enabled       bool `json:"enabled"`
	ClearThinking bool `json:"clear_thinking"`
}

type Provider struct {
	Name                 string
	URL                  string
	BaseURL              string
	APIKey               string
	RoutePrefixes        []string
	AuthStyle            string
	ApiFormat            string
	TimeoutSeconds       int
	RetryableStatusCodes []int
	Thinking             *ThinkingConfig
}

type Config struct {
	Server         ServerConfig               `json:"server"`
	Governance     GovernanceConfig           `json:"governance"`
	SecurityFilter SecurityFilterConfig       `json:"security_filter"`
	PrefixCache    PrefixCacheConfig          `json:"prefix_cache"`
	Compaction     CompactionConfig           `json:"compaction"`
	Window         WindowConfig               `json:"window"`
	ResponseCache  ResponseCacheConfig        `json:"response_cache"`
	Discovery      DiscoveryConfig            `json:"discovery"`
	MCPServers     map[string]MCPServerConfig `json:"mcp_servers,omitempty"`
	Agents         map[string]AgentConfig     `json:"agents"`
	Providers      map[string]ProviderConfig  `json:"providers"`
}

type ServerConfig struct {
	ListenAddr   string `json:"listen_addr"`
	MaxBodyBytes int64  `json:"max_body_bytes"`
	UserAgent    string `json:"user_agent"`
	LogLevel     string `json:"log_level"`
}

type GovernanceConfig struct {
	BlockedExecutionPatterns []string `json:"blocked_execution_patterns"`
	RatelimitMaxRPM          int      `json:"ratelimit_max_rpm"`
	RatelimitMaxTPM          int      `json:"ratelimit_max_tpm"`
	TruncationStrategy       string   `json:"truncation_strategy"`
	KeepFirstPercent         float64  `json:"keep_first_percent"`
	KeepLastPercent          float64  `json:"keep_last_percent"`
	RetryableStatusCodes     []int    `json:"retryable_status_codes"`
	TFIDFQuerySource         string   `json:"tfidf_query_source"`
	AutoContextSkip          bool     `json:"auto_context_skip"`
	AutoReorderByLatency     bool     `json:"auto_reorder_by_latency"`
	RoutingStrategy          string   `json:"routing_strategy"`
	RoutingLatencyWeight     float64  `json:"routing_latency_weight"`
	RoutingCostWeight        float64  `json:"routing_cost_weight"`
	MaxCostPerRequest        float64  `json:"max_cost_per_request"`
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

type EngineRef struct {
	AgentName        string         `json:"-"`
	Provider         string         `json:"provider"`
	Model            string         `json:"model"`
	SystemPrompt     string         `json:"system_prompt"`
	SystemPromptFile string         `json:"system_prompt_file"`
	TimeoutSeconds   int            `json:"timeout_seconds"`
	ResolvedTargets  []EngineTarget `json:"-"`
}

func (e *EngineRef) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		e.AgentName = s
		return nil
	}

	type alias EngineRef
	aux := (*alias)(e)
	return json.Unmarshal(data, aux)
}

// EngineTarget describes a resolved engine target with timeout and configuration.
type EngineTarget struct {
	Engine   EngineConfig
	Provider *Provider
}

type SecurityFilterConfig struct {
	Enabled             bool      `json:"enabled"`
	RedactionLabel      string    `json:"redaction_label"`
	Patterns            []string  `json:"patterns"`
	OutputEnabled       bool      `json:"output_enabled"`
	OutputWindowChars   int       `json:"output_window_chars"`
	SkipOnEngineFailure bool      `json:"skip_on_engine_failure"`
	Engine              EngineRef `json:"engine"`
	EntropyEnabled      bool      `json:"entropy_enabled"`
	EntropyThreshold    float64   `json:"entropy_threshold"`
	EntropyMinToken     int       `json:"entropy_min_token"`
	enabledSet          bool      `json:"-"`
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
	PruneStaleTools        bool `json:"prune_stale_tools"`
	ToolProtectionWindow   int  `json:"tool_protection_window"`
	PruneThoughts          bool `json:"prune_thoughts"`
	enabledSet             bool `json:"-"`
	minifySet              bool `json:"-"`
	collapseSet            bool `json:"-"`
	trimSet                bool `json:"-"`
	normalizeSet           bool `json:"-"`
	pruneSet               bool `json:"-"`
	pruneThoughtsSet       bool `json:"-"`
}

func (c *CompactionConfig) EnabledWasSet() bool       { return c.enabledSet }
func (c *CompactionConfig) MinifyWasSet() bool        { return c.minifySet }
func (c *CompactionConfig) CollapseWasSet() bool      { return c.collapseSet }
func (c *CompactionConfig) TrimWasSet() bool          { return c.trimSet }
func (c *CompactionConfig) NormWasSet() bool          { return c.normalizeSet }
func (c *CompactionConfig) PruneWasSet() bool         { return c.pruneSet }
func (c *CompactionConfig) PruneThoughtsWasSet() bool { return c.pruneThoughtsSet }

func (c *CompactionConfig) UnmarshalJSON(data []byte) error {
	type alias CompactionConfig
	aux := struct {
		Enabled                *bool `json:"enabled"`
		JSONMinify             *bool `json:"json_minify"`
		CollapseBlankLines     *bool `json:"collapse_blank_lines"`
		TrimTrailingWhitespace *bool `json:"trim_trailing_whitespace"`
		NormalizeLineEndings   *bool `json:"normalize_line_endings"`
		PruneStaleTools        *bool `json:"prune_stale_tools"`
		ToolProtectionWindow   *int  `json:"tool_protection_window"`
		PruneThoughts          *bool `json:"prune_thoughts"`
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
	if aux.PruneStaleTools != nil {
		c.PruneStaleTools = *aux.PruneStaleTools
		c.pruneSet = true
	}
	if aux.ToolProtectionWindow != nil {
		c.ToolProtectionWindow = *aux.ToolProtectionWindow
	}
	if aux.PruneThoughts != nil {
		c.PruneThoughts = *aux.PruneThoughts
		c.pruneThoughtsSet = true
	}
	return nil
}

type WindowConfig struct {
	Enabled         bool      `json:"enabled"`
	Mode            string    `json:"mode"`
	ActiveMessages  int       `json:"active_messages"`
	TriggerRatio    float64   `json:"trigger_ratio"`
	SummaryMaxRunes int       `json:"summary_max_runes"`
	MaxContext      int       `json:"max_context"`
	Engine          EngineRef `json:"engine"`
}

type MCPServerConfig struct {
	URL               string            `json:"url"`
	Headers           map[string]string `json:"headers,omitempty"`
	Timeout           int               `json:"timeout,omitempty"`
	KeepAliveInterval int               `json:"keep_alive_interval,omitempty"`
}

type AgentMCPConfig struct {
	Servers       []string `json:"servers"`
	MaxIterations int      `json:"max_iterations,omitempty"`
	AutoSave      bool     `json:"auto_save,omitempty"`
	AutoSearch    bool     `json:"auto_search,omitempty"`
	SearchTool    string   `json:"search_tool,omitempty"`
	SaveTool      string   `json:"save_tool,omitempty"`
}
