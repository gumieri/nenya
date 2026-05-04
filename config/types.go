package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// AgentModel defines a single model entry within an agent's model list,
// specifying the provider, model name, URL override, and context limits.
// Supports static entries (provider/model) and dynamic entries (provider_rgx/model_rgx)
// that expand against the discovery catalog at runtime.
type AgentModel struct {
	Provider             string   `json:"provider"`
	Model                string   `json:"model"`
	Format               string   `json:"format,omitempty"`
	URL                  string   `json:"url"`
	MaxContext           int      `json:"max_context"`
	MaxOutput            int      `json:"max_output"`
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`
	ProviderRgx          string   `json:"provider_rgx,omitempty"`
	ModelRgx             string   `json:"model_rgx,omitempty"`

	providerRE *regexp.Regexp
	modelRE    *regexp.Regexp
}

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

func (m *AgentModel) MatchesCatalog(provider, model string) bool {
	if m.Provider != "" && m.Provider != provider {
		return false
	}
	if m.Model != "" && m.Model != model {
		return false
	}
	if m.providerRE != nil && !m.providerRE.MatchString(provider) {
		return false
	}
	if m.modelRE != nil && !m.modelRE.MatchString(model) {
		return false
	}
	return true
}

func (m *AgentModel) IsDynamic() bool {
	return m.providerRE != nil || m.modelRE != nil
}

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
				Provider:   "",
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
	URL                  string            `json:"url"`
	AuthStyle            string            `json:"auth_style"`
	ApiFormat            string            `json:"api_format"`
	FormatURLs           map[string]string `json:"format_urls,omitempty"`
	TimeoutSeconds       int               `json:"timeout_seconds"`
	RetryableStatusCodes []int             `json:"retryable_status_codes"`
	MaxRetryAttempts     int               `json:"max_retry_attempts"`
	Thinking             *ThinkingConfig   `json:"thinking,omitempty"`
}

type ThinkingConfig struct {
	Enabled       bool `json:"enabled"`
	ClearThinking bool `json:"clear_thinking"`
}

type Provider struct {
	Name                 string
	URL                  string
	BaseURL              string
	APIKey               string
	AuthStyle            string
	ApiFormat            string
	FormatURLs           map[string]string
	TimeoutSeconds       int
	RetryableStatusCodes []int
	MaxRetryAttempts     int
	Thinking             *ThinkingConfig
}

type Config struct {
	Server        ServerConfig               `json:"server"`
	Context       ContextConfig              `json:"context"`
	Governance    GovernanceConfig           `json:"governance"`
	Bouncer       BouncerConfig              `json:"bouncer,omitempty"`
	PrefixCache   PrefixCacheConfig          `json:"prefix_cache,omitempty"`
	Compaction    CompactionConfig           `json:"compaction,omitempty"`
	Window        WindowConfig               `json:"window,omitempty"`
	ResponseCache ResponseCacheConfig        `json:"response_cache,omitempty"`
	Discovery     DiscoveryConfig            `json:"discovery,omitempty"`
	MCPServers    map[string]MCPServerConfig `json:"mcp_servers,omitempty"`
	Agents        map[string]AgentConfig     `json:"agents,omitempty"`
	Providers     map[string]ProviderConfig  `json:"providers,omitempty"`
}

type ServerConfig struct {
	ListenAddr           string `json:"listen_addr"`
	MaxBodyBytes         int64  `json:"max_body_bytes"`
	UserAgent            string `json:"user_agent"`
	LogLevel             string `json:"log_level"`
	SecureMemoryRequired *bool  `json:"secure_memory_required"`
}

func (s *ServerConfig) SecureMemoryRequiredWasSet() bool { return s.SecureMemoryRequired != nil }

type ContextConfig struct {
	TruncationStrategy     string  `json:"truncation_strategy"`
	TruncationKeepFirstPct float64 `json:"truncation_keep_first_pct"`
	TruncationKeepLastPct  float64 `json:"truncation_keep_last_pct"`
	TFIDFQuerySource       string  `json:"tfidf_query_source"`
	AutoContextSkip        *bool   `json:"auto_context_skip,omitempty"`
	AutoReorderByLatency   *bool   `json:"auto_reorder_by_latency,omitempty"`
}

func (c *ContextConfig) AutoContextSkipSet() bool      { return c.AutoContextSkip != nil }
func (c *ContextConfig) AutoReorderByLatencySet() bool { return c.AutoReorderByLatency != nil }

type GovernanceConfig struct {
	BlockedExecutionPatterns []string `json:"blocked_execution_patterns"`
	RatelimitMaxRPM          *int     `json:"ratelimit_max_rpm,omitempty"`
	RatelimitMaxTPM          *int     `json:"ratelimit_max_tpm,omitempty"`
	RetryableStatusCodes     []int    `json:"retryable_status_codes"`
	MaxRetryAttempts         int      `json:"max_retry_attempts"`
	RoutingStrategy          string   `json:"routing_strategy"`
	RoutingLatencyWeight     float64  `json:"routing_latency_weight"`
	RoutingCostWeight        float64  `json:"routing_cost_weight"`
	MaxCostPerRequest        float64  `json:"max_cost_per_request"`
	EmptyStreamAsError       *bool    `json:"empty_stream_as_error,omitempty"`
}

func (g *GovernanceConfig) RPMSet() bool                { return g.RatelimitMaxRPM != nil }
func (g *GovernanceConfig) TPMSet() bool                { return g.RatelimitMaxTPM != nil }
func (g *GovernanceConfig) EmptyStreamAsErrorSet() bool { return g.EmptyStreamAsError != nil }

func (g *GovernanceConfig) EffectiveMaxRetryAttempts() int {
	if g.MaxRetryAttempts > 0 {
		return g.MaxRetryAttempts
	}
	return 3
}

type SecretsConfig struct {
	ClientToken  string            `json:"client_token,omitempty"`
	ProviderKeys map[string]string `json:"provider_keys,omitempty"`
	ApiKeys      map[string]ApiKey `json:"api_keys,omitempty"`
}

type ApiKey struct {
	Name          string         `json:"name"`
	Token         string         `json:"token"`
	Roles         []string       `json:"roles"`
	AllowedAgents []string       `json:"allowed_agents"`
	CreatedAt     string         `json:"created_at,omitempty"`
	ExpiresAt     string         `json:"expires_at,omitempty"`
	Enabled       bool           `json:"enabled"`
	Permissions   map[string]any `json:"permissions,omitempty"`
}

func (k *ApiKey) Validate() error {
	if k.Token == "" {
		return errors.New("token cannot be empty")
	}
	if len(k.Token) < 16 {
		return errors.New("token too short (minimum 16 characters)")
	}
	if len(k.Roles) == 0 {
		return errors.New("at least one role is required")
	}
	for _, role := range k.Roles {
		if !isValidRole(role) {
			return fmt.Errorf("invalid role: %q", role)
		}
	}
	if k.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, k.ExpiresAt); err != nil {
			return fmt.Errorf("invalid expires_at format (use RFC3339): %w", err)
		}
	}
	return nil
}

const (
	RoleAdmin    = "admin"
	RoleUser     = "user"
	RoleReadOnly = "read-only"
)

func isValidRole(role string) bool {
	switch role {
	case RoleAdmin, RoleUser, RoleReadOnly:
		return true
	default:
		return false
	}
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
	Provider         string         `json:"provider,omitempty"`
	Model            string         `json:"model,omitempty"`
	SystemPrompt     string         `json:"system_prompt,omitempty"`
	SystemPromptFile string         `json:"system_prompt_file,omitempty"`
	TimeoutSeconds   int            `json:"timeout_seconds,omitempty"`
	ResolvedTargets  []EngineTarget `json:"-"`
}

func (e *EngineRef) UnmarshalJSON(data []byte) error {
	// First try unmarshaling as a raw string (for shorthand forms)
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if strings.Contains(s, "/") {
			parts := strings.SplitN(s, "/", 2)
			e.Provider = parts[0]
			e.Model = parts[1]
		} else {
			e.AgentName = s
		}
		return nil
	}

	// Fallback to object unmarshaling
	type alias EngineRef
	aux := (*alias)(e)
	return json.Unmarshal(data, aux)
}

type EngineTarget struct {
	Engine   EngineConfig
	Provider *Provider
}

type BouncerConfig struct {
	Enabled            *bool     `json:"enabled,omitempty"`
	RedactionLabel     string    `json:"redaction_label"`
	RedactPreset       string    `json:"redact_preset,omitempty"`
	RedactPatterns     []string  `json:"redact_patterns,omitempty"`
	RedactOutput       bool      `json:"redact_output,omitempty"`
	RedactOutputWindow int       `json:"redact_output_window,omitempty"`
	FailOpen           *bool     `json:"fail_open,omitempty"`
	Engine             EngineRef `json:"engine,omitempty"`
	EntropyEnabled     bool      `json:"entropy_enabled,omitempty"`
	EntropyThreshold   float64   `json:"entropy_threshold,omitempty"`
	EntropyMinToken    int       `json:"entropy_min_token,omitempty"`
}

func (s *BouncerConfig) EnabledWasSet() bool  { return s.Enabled != nil }
func (s *BouncerConfig) FailOpenWasSet() bool { return s.FailOpen != nil }

func (s *BouncerConfig) UnmarshalJSON(data []byte) error {
	type alias BouncerConfig
	aux := &struct {
		RedactPatterns []string `json:"redact_patterns"`
		*alias
	}{
		alias: (*alias)(s),
	}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if aux.Enabled == nil && len(aux.RedactPatterns) > 0 {
		s.Enabled = PtrTo(true)
	}
	return nil
}

type PrefixCacheConfig struct {
	Enabled               bool  `json:"enabled"`
	PinSystemFirst        *bool `json:"pin_system_first,omitempty"`
	StableTools           *bool `json:"stable_tools,omitempty"`
	SkipRedactionOnSystem *bool `json:"skip_redaction_on_system,omitempty"`
}

func (c *PrefixCacheConfig) PinWasSet() bool           { return c.PinSystemFirst != nil }
func (c *PrefixCacheConfig) StableWasSet() bool        { return c.StableTools != nil }
func (c *PrefixCacheConfig) SkipRedactionWasSet() bool { return c.SkipRedactionOnSystem != nil }

type CompactionConfig struct {
	Enabled                *bool `json:"enabled,omitempty"`
	JSONMinify             *bool `json:"json_minify,omitempty"`
	CollapseBlankLines     *bool `json:"collapse_blank_lines,omitempty"`
	TrimTrailingWhitespace *bool `json:"trim_trailing_whitespace,omitempty"`
	NormalizeLineEndings   *bool `json:"normalize_line_endings,omitempty"`
	PruneStaleTools        *bool `json:"prune_stale_tools,omitempty"`
	ToolProtectionWindow   int   `json:"tool_protection_window"`
	PruneThoughts          *bool `json:"prune_thoughts,omitempty"`
}

func (c *CompactionConfig) EnabledWasSet() bool       { return c.Enabled != nil }
func (c *CompactionConfig) MinifyWasSet() bool        { return c.JSONMinify != nil }
func (c *CompactionConfig) CollapseWasSet() bool      { return c.CollapseBlankLines != nil }
func (c *CompactionConfig) TrimWasSet() bool          { return c.TrimTrailingWhitespace != nil }
func (c *CompactionConfig) NormWasSet() bool          { return c.NormalizeLineEndings != nil }
func (c *CompactionConfig) PruneWasSet() bool         { return c.PruneStaleTools != nil }
func (c *CompactionConfig) PruneThoughtsWasSet() bool { return c.PruneThoughts != nil }

type WindowConfig struct {
	Enabled         bool      `json:"enabled"`
	Mode            string    `json:"mode"`
	ActiveMessages  int       `json:"active_messages"`
	TriggerRatio    float64   `json:"trigger_ratio"`
	SummaryMaxRunes int       `json:"summary_max_runes"`
	MaxContext      int       `json:"max_context"`
	Engine          EngineRef `json:"engine"`
	KeepFirstPct    float64   `json:"keep_first_pct"`
	KeepLastPct     float64   `json:"keep_last_pct"`
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

type DiscoveryConfig struct {
	Enabled          *bool             `json:"enabled,omitempty"`
	AutoAgents       *bool             `json:"auto_agents,omitempty"`
	AutoAgentsConfig *AutoAgentsConfig `json:"auto_agents_config,omitempty"`
}

type AutoAgentCategoryConfig struct {
	Enabled bool `json:"enabled"`
}

type AutoAgentsConfig struct {
	Fast      *AutoAgentCategoryConfig `json:"fast,omitempty"`
	Reasoning *AutoAgentCategoryConfig `json:"reasoning,omitempty"`
	Vision    *AutoAgentCategoryConfig `json:"vision,omitempty"`
	Tools     *AutoAgentCategoryConfig `json:"tools,omitempty"`
	Large     *AutoAgentCategoryConfig `json:"large,omitempty"`
	Balanced  *AutoAgentCategoryConfig `json:"balanced,omitempty"`
	Coding    *AutoAgentCategoryConfig `json:"coding,omitempty"`
}

func (a *AutoAgentsConfig) IsEnabled(category string) bool {
	if a == nil {
		return true
	}
	var cfg *AutoAgentCategoryConfig
	switch category {
	case "fast":
		cfg = a.Fast
	case "reasoning":
		cfg = a.Reasoning
	case "vision":
		cfg = a.Vision
	case "tools":
		cfg = a.Tools
	case "large":
		cfg = a.Large
	case "balanced":
		cfg = a.Balanced
	case "coding":
		cfg = a.Coding
	default:
		return false
	}
	if cfg == nil {
		return false
	}
	return cfg.Enabled
}

func (d *DiscoveryConfig) EnabledWasSet() bool    { return d.Enabled != nil }
func (d *DiscoveryConfig) AutoAgentsWasSet() bool { return d.AutoAgents != nil }

type ResponseCacheConfig struct {
	Enabled            *bool  `json:"enabled,omitempty"`
	MaxEntries         int    `json:"max_entries"`
	MaxEntryBytes      int64  `json:"max_entry_bytes"`
	TTLSeconds         int    `json:"ttl_seconds"`
	EvictEverySeconds  int    `json:"evict_every_seconds"`
	ForceRefreshHeader string `json:"force_refresh_header"`
}

func (c *ResponseCacheConfig) EnabledWasSet() bool { return c.Enabled != nil }

func PtrTo[T any](v T) *T { return &v }
