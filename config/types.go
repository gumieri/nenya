package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const MaxTimeoutSeconds = 86400 // 24 hours

const (
	CacheModeExplicit  = "explicit"
	CacheModeAutomatic = "automatic"
	OpenAIModeImplicit = "implicit"
	OpenAIModeExplicit = "explicit"
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
	ReasoningEffort      string   `json:"reasoning_effort,omitempty"`

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

// AgentConfig defines the configuration for a named AI agent within the
// gateway. It includes strategy selection, circuit breaker thresholds,
// model lists, MCP tool integration, and prompt configuration.
type AgentConfig struct {
	Strategy                string          `json:"strategy"`
	Description             string          `json:"description,omitempty"`
	CooldownSeconds         int             `json:"cooldown_seconds"`
	FailureThreshold        int             `json:"failure_threshold"`
	FailureWindowSec        int             `json:"failure_window_secs"`
	SuccessThreshold        int             `json:"success_threshold"`
	MaxRetries              int             `json:"max_retries"`
	SystemPrompt            string          `json:"system_prompt"`
	SystemPromptFile        string          `json:"system_prompt_file"`
	ForceSystemPrompt       bool            `json:"force_system_prompt"`
	Models                  []AgentModel    `json:"models,omitempty"`
	MCP                     *AgentMCPConfig `json:"mcp,omitempty"`
	BudgetLimitUSD          float64         `json:"budget_limit_usd,omitempty"`
	CacheSalt               *string         `json:"cache_salt,omitempty"`
	StickySessionTTLSeconds int             `json:"sticky_session_ttl_seconds,omitempty"`
	// StickyProvider controls cache-affinity failover for this agent:
	// "" / "off" (default) fail over freely; "lenient" fails over only
	// when the failure is a 5xx, the target's circuit was open, or a
	// context-limit summarization retry is available; "strict" never
	// fails over to another target (same-target backoff retries and the
	// summarization retry still run).
	StickyProvider string `json:"sticky_provider,omitempty"`
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

// RequestScopedErrorRule teaches the gateway that a provider's error shape
// (status + case-insensitive body substring) is actually the client's fault,
// even when the status class suggests otherwise (e.g. an invalid-parameter
// 401). Request-scoped errors never cool down or rotate credentials; the
// error goes straight back to the client.
type RequestScopedErrorRule struct {
	// Status is the HTTP status code the rule applies to (0 = any status).
	Status int `json:"status"`
	// MessagePattern is a case-insensitive substring matched against the
	// error body.
	MessagePattern string `json:"message_pattern"`
}

// ProviderConfig defines the wire-level configuration for an upstream LLM
// provider: the endpoint URL, authentication style, API format, timeouts,
// retry settings, and per-provider rate limits. User-provided configs override
// built-in defaults.
type ProviderConfig struct {
	URL        string            `json:"url"`
	AuthStyle  string            `json:"auth_style"`
	ApiFormat  string            `json:"api_format"`
	FormatURLs map[string]string `json:"format_urls,omitempty"`
	// TimeoutSeconds is the total request deadline for this provider.
	TimeoutSeconds int `json:"timeout_seconds"`
	// ResponseHeaderTimeoutSeconds is the transport-level time-to-first-byte
	// bound (HTTP ResponseHeaderTimeout) for dispatches to this provider.
	// When 0, falls back to TimeoutSeconds; when both are 0, uses
	// DefaultResponseHeaderTimeoutSeconds (30s). Unlike TimeoutSeconds it
	// also applies to streaming requests, whose context is otherwise
	// unbounded. Capped at 86400 (24 hours) by validation.
	ResponseHeaderTimeoutSeconds int `json:"response_header_timeout_seconds,omitempty"`
	// StreamIdleTimeoutSeconds is the stall detection timeout for SSE streams.
	// When 0, uses the global governance default. When > 0, overrides the global
	// default. Capped at 86400 (24 hours) by validation.
	StreamIdleTimeoutSeconds int `json:"stream_idle_timeout_seconds,omitempty"`
	// RetryableStatusCodes defines which HTTP status codes trigger automatic retry.
	RetryableStatusCodes []int `json:"retryable_status_codes"`
	// MaxRetryAttempts is the maximum number of retry attempts for failed requests.
	MaxRetryAttempts int `json:"max_retry_attempts"`
	// RetryablePhrases are case-insensitive substrings that make a 4xx error
	// body retryable for this provider (e.g. aggregator-relayed upstream
	// failures). Matched in addition to the built-in pattern sets; empty
	// list means built-in sets only.
	RetryablePhrases []string `json:"retryable_phrases,omitempty"`
	// RequestScopedErrors classifies provider error shapes as the client's
	// fault (NENYA-42): matching errors bypass cooldown/rotation state and
	// surface directly to the client. Empty uses the default ErrorKind
	// scope classification. Note: context-length handling (summarization
	// retry) takes precedence over rules.
	RequestScopedErrors []RequestScopedErrorRule `json:"request_scoped_errors,omitempty"`
	// Thinking configures reasoning token behavior for this provider.
	Thinking *ThinkingConfig `json:"thinking,omitempty"`
	// APIKey is the authentication key (typically loaded from secrets).
	APIKey string `json:"api_key,omitempty"`
	// Accounts defines multi-account rotation for this provider.
	Accounts []AccountConfig `json:"accounts,omitempty"`
	// RatelimitMaxRPM is the requests-per-minute rate limit.
	RatelimitMaxRPM *int `json:"ratelimit_max_rpm,omitempty"`
	// RatelimitMaxTPM is the tokens-per-minute rate limit.
	RatelimitMaxTPM *int `json:"ratelimit_max_tpm,omitempty"`
	// MaxConcurrentRequests caps the number of in-flight requests dispatched
	// to this provider (0 or omitted = unlimited). Providers like the Z.AI
	// Coding Plan enforce per-model concurrency limits; requests over the
	// cap queue until a slot frees or the client context is canceled.
	MaxConcurrentRequests int `json:"max_concurrent_requests,omitempty"`
	// ModelConcurrency overrides MaxConcurrentRequests per model ID served
	// by this provider (e.g. {"glm-5.3": 5, "glm-5.3-flash": 50}). Values
	// must be non-negative; 0 = unlimited for that model.
	ModelConcurrency map[string]int `json:"model_concurrency,omitempty"`
	// ModelAliases rewrites model IDs at dispatch time for this provider:
	// key = canonical ID (what the client sends / catalog lists), value =
	// physical ID (what the upstream receives). Exact match on the
	// canonical ID. Use when a gateway/provider expects a different
	// spelling (e.g. dotted→dashed slugs) without per-provider code.
	ModelAliases map[string]string `json:"model_aliases,omitempty"`
	// TokenBudgetDaily caps the token estimate this provider may serve per
	// UTC day (0 = unlimited). Keys whose budget_tier is "fill" are
	// rejected once the budget is exhausted; "always" keys are served.
	TokenBudgetDaily int `json:"token_budget_daily,omitempty"`
	// StreamBootstrapBufferBytes opts this provider into stream bootstrap
	// buffering (NENYA-45): SSE handshake events are held (up to this many
	// bytes) until the stream is decidable — a real output event flushes the
	// buffer, an in-stream rejection fails over to the next target before
	// the 200 is committed. nil = inherit the governance global; 0 = force
	// off; >0 = enable with that budget.
	StreamBootstrapBufferBytes *int `json:"stream_bootstrap_buffer_bytes,omitempty"`
	// SessionStickyKeys opts out of session-sticky credential pinning
	// (NENYA-29): sessions deterministically reuse one account so upstream
	// per-key prompt caches stay warm under multi-account rotation. nil
	// (default) = enabled; false = this provider always rotates via LRU.
	SessionStickyKeys *bool `json:"session_sticky_keys,omitempty"`
	// ThoughtSignaturePolicy selects the Gemini unsigned-signature policy
	// (NENYA-50): "strip" (default) removes unsigned tool calls with their
	// paired responses on Gemini 3+, "placeholder" injectates the
	// upstream-tolerated skip placeholder instead of stripping,
	// "passthrough" forwards unsigned history. Pre-3 models keep history
	// under every policy. Empty means "strip".
	ThoughtSignaturePolicy *string `json:"thought_signature_policy,omitempty"`
	// Billing configures usage-based billing tracking for this provider.
	Billing *BillingConfig `json:"billing,omitempty"`
	// AllowedModels is a list of RE2 regex patterns that models from this
	// provider must match to be included in the catalog. Empty or omitted
	// means all models are allowed (default behavior). Patterns are unanchored
	// MatchString semantics — anchor with ^...$ for exact pinning.
	AllowedModels []string `json:"allowed_models,omitempty"`
}

// AccountConfig defines a single credential/account for multi-account providers.
type AccountConfig struct {
	ID string `json:"id"`
	// Weight is the relative traffic share for multi-account rotation
	// (NENYA-40); a weight-2 account receives twice the traffic of a
	// weight-1 account. Values below 1 are treated as 1.
	Weight     int    `json:"weight,omitempty"`
	Type       string `json:"type"`
	Credential string `json:"credential"`
}

// ThinkingConfig controls whether reasoning/thinking tokens are requested
// from a provider and whether they are stripped from the response.
type ThinkingConfig struct {
	Enabled       bool `json:"enabled"`
	ClearThinking bool `json:"clear_thinking"`
}

// AuthStyle values for Provider.AuthStyle. AuthStyleNone indicates an
// endpoint that requires no Authorization header (e.g. a local Ollama).
// Other supported values (bearer, azure, bearer+x-goog, anthropic) are
// applied by the adapter layer (adapter.AdapterForAuthStyle), which also
// validates the style at request time.
const AuthStyleNone = "none"

// Provider is the resolved runtime representation of a provider,
// combining user config with secrets and derived defaults. Created by
// ResolveProviders at startup.
type Provider struct {
	Name           string
	URL            string
	BaseURL        string
	APIKey         string
	AuthStyle      string
	ApiFormat      string
	FormatURLs     map[string]string
	TimeoutSeconds int
	// ResponseHeaderTimeoutSeconds is the transport-level time-to-first-byte
	// bound (HTTP ResponseHeaderTimeout) for dispatches to this provider.
	// When 0, EffectiveResponseHeaderTimeout falls back to TimeoutSeconds,
	// then to DefaultResponseHeaderTimeoutSeconds.
	ResponseHeaderTimeoutSeconds int
	// StreamIdleTimeoutSeconds is the stall detection timeout for SSE streams.
	// When 0, uses the global governance default. When > 0, overrides the global default.
	StreamIdleTimeoutSeconds int
	RetryableStatusCodes     []int
	MaxRetryAttempts         int
	RetryablePhrases         []string
	RequestScopedErrors      []RequestScopedErrorRule
	Thinking                 *ThinkingConfig
	Billing                  *BillingConfig
	AllowedModels            []string
	allowedRE                []*regexp.Regexp
	// MaxConcurrentRequests caps in-flight requests dispatched to this
	// provider (0 = unlimited). See ProviderConfig.MaxConcurrentRequests.
	MaxConcurrentRequests int
	// ModelConcurrency overrides MaxConcurrentRequests per model ID.
	ModelConcurrency map[string]int
	// ModelAliases rewrites model IDs at dispatch time (canonical →
	// physical). See ProviderConfig.ModelAliases.
	ModelAliases map[string]string
	// TokenBudgetDaily caps the token estimate this provider may serve per
	// UTC day (0 = unlimited). See ProviderConfig.TokenBudgetDaily.
	TokenBudgetDaily int
	// StreamBootstrapBufferBytes is the per-provider stream bootstrap
	// buffering opt-in. See ProviderConfig.StreamBootstrapBufferBytes.
	StreamBootstrapBufferBytes *int
	// ThoughtSignaturePolicy selects the Gemini unsigned-signature policy
	// (NENYA-50): strip | placeholder | passthrough. See
	// ProviderConfig.ThoughtSignaturePolicy.
	ThoughtSignaturePolicy string
	// SessionStickyKeys opts out of session-sticky credential pinning.
	// See ProviderConfig.SessionStickyKeys.
	SessionStickyKeys *bool
}

// DefaultResponseHeaderTimeoutSeconds is the default transport-level
// time-to-first-byte bound for upstream providers when neither
// response_header_timeout_seconds nor timeout_seconds is configured.
const DefaultResponseHeaderTimeoutSeconds = 30

// EffectiveResponseHeaderTimeout resolves the transport-level
// time-to-first-byte bound for dispatches to this provider:
// response_header_timeout_seconds when positive, else timeout_seconds
// when positive, else DefaultResponseHeaderTimeoutSeconds.
func (p *Provider) EffectiveResponseHeaderTimeout() time.Duration {
	if p == nil {
		return DefaultResponseHeaderTimeoutSeconds * time.Second
	}
	if p.ResponseHeaderTimeoutSeconds > 0 {
		return time.Duration(p.ResponseHeaderTimeoutSeconds) * time.Second
	}
	if p.TimeoutSeconds > 0 {
		return time.Duration(p.TimeoutSeconds) * time.Second
	}
	return DefaultResponseHeaderTimeoutSeconds * time.Second
}

// ConcurrencyLimit resolves the in-flight request cap for a model served by
// this provider: per-model override first, then the provider-wide cap.
// A return value of 0 means unlimited.
func (p *Provider) ConcurrencyLimit(model string) int {
	if p == nil {
		return 0
	}
	if limit, ok := p.ModelConcurrency[model]; ok {
		return limit
	}
	return p.MaxConcurrentRequests
}

// AllowsModel returns true if the provider allows the given model ID.
// Empty allowed_models means all models are allowed.
func (p *Provider) AllowsModel(id string) bool {
	if len(p.allowedRE) == 0 {
		return true
	}
	for _, re := range p.allowedRE {
		if re.MatchString(id) {
			return true
		}
	}
	return false
}

// CompileAllowedModels compiles a list of regex patterns for provider model
// allowlist validation. Returns compiled regexes or an error on invalid pattern.
func CompileAllowedModels(patterns []string) ([]*regexp.Regexp, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	res := make([]*regexp.Regexp, len(patterns))
	for i, pat := range patterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("invalid allowed_models pattern %q: %w", pat, err)
		}
		res[i] = re
	}
	return res, nil
}

// Config is the top-level configuration for the Nenya gateway. It is
// loaded from a JSON file (or merged from config.d/*.json) and populated
// with built-in provider defaults before validation.
type Config struct {
	Server        ServerConfig               `json:"server"`
	Debug         DebugConfig                `json:"debug,omitempty"`
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
	LocalEngine   *LocalEngineConfig         `json:"local_engine,omitempty"`
}

// DebugConfig controls debug and profiling features.
type DebugConfig struct {
	PprofEnabled *bool `json:"pprof_enabled,omitempty"`
}

func (d *DebugConfig) PprofEnabledWasSet() bool { return wasSet(d.PprofEnabled) }

// LocalEngineConfig defines the configuration for the local Ollama engine lifecycle manager.
type LocalEngineConfig struct {
	BaseURL        string   `json:"base_url"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	MaxSessions    int      `json:"max_sessions"`
	AutoLoad       bool     `json:"auto_load"`
	StartupModels  []string `json:"startup_models"`
}

// ServerConfig defines the HTTP server settings: listen address, body
// size limits, user agent string, log level, and secure memory policy.
type ServerConfig struct {
	ListenAddr           string `json:"listen_addr"`
	MaxBodyBytes         int64  `json:"max_body_bytes"`
	UserAgent            string `json:"user_agent"`
	LogLevel             string `json:"log_level"`
	SecureMemoryRequired *bool  `json:"secure_memory_required"`
}

// wasSet returns true if v is non-nil (field was explicitly set by user).
// This distinguishes "not set" (nil) from "explicitly set to zero" (pointer to 0).
func wasSet[T any](v *T) bool { return v != nil }

func (s *ServerConfig) SecureMemoryRequiredWasSet() bool { return wasSet(s.SecureMemoryRequired) }

// ContextConfig controls how message context is truncated before
// forwarding upstream. Supports middle-out truncation with configurable
// keep percentages and TF-IDF query source for relevance scoring.
type ContextConfig struct {
	TruncationStrategy     string  `json:"truncation_strategy"`
	TruncationKeepFirstPct float64 `json:"truncation_keep_first_pct"`
	TruncationKeepLastPct  float64 `json:"truncation_keep_last_pct"`
	TFIDFQuerySource       string  `json:"tfidf_query_source"`
	HardLimitTokens        int     `json:"hard_limit_tokens,omitempty"`
}

// GovernanceConfig defines security, rate-limiting, and routing policies
// for the gateway: blocked execution patterns, retry behavior, circuit
// breaker thresholds, latency- and cost-weighted routing, and auto-tuning flags.
type GovernanceConfig struct {
	BlockedExecutionPatterns []string `json:"blocked_execution_patterns"`
	RatelimitMaxRPM          *int     `json:"ratelimit_max_rpm,omitempty"`
	RatelimitMaxTPM          *int     `json:"ratelimit_max_tpm,omitempty"`
	// MaxConcurrentRequests is the global fallback cap on in-flight requests
	// per provider+model when the provider config does not set its own limit
	// (0 or omitted = unlimited).
	MaxConcurrentRequests int     `json:"max_concurrent_requests,omitempty"`
	RetryableStatusCodes  []int   `json:"retryable_status_codes"`
	MaxRetryAttempts      int     `json:"max_retry_attempts"`
	RoutingStrategy       string  `json:"routing_strategy"`
	RoutingLatencyWeight  float64 `json:"routing_latency_weight"`
	RoutingCostWeight     float64 `json:"routing_cost_weight"`
	MaxCostPerRequest     float64 `json:"max_cost_per_request"`
	EmptyStreamAsError    *bool   `json:"empty_stream_as_error,omitempty"`
	// EarlyStreamErrorFailover fails over to the next target when the first
	// SSE event of a stream is an upstream error, before headers are committed.
	EarlyStreamErrorFailover *bool `json:"early_stream_error_failover,omitempty"`
	AutoContextSkip          *bool `json:"auto_context_skip,omitempty"`
	AutoReorderByLatency     *bool `json:"auto_reorder_by_latency,omitempty"`
	HalfOpenMaxRequests      int   `json:"half_open_max_requests,omitempty"`
	// MinQuotaCooldownSeconds floors quota-class cooldowns (NENYA-41):
	// upstreams sometimes send sub-second Retry-After values on quota errors,
	// and honoring them literally produces zero-wait retry storms against the
	// same exhausted account. 0 or unset applies the built-in default (10s);
	// a negative value disables the floor.
	MinQuotaCooldownSeconds int   `json:"min_quota_cooldown_seconds,omitempty"`
	AutoRetryOnContextLimit *bool `json:"auto_retry_on_context_limit,omitempty"`
	// ParamCompat declares parameter-compatibility rules for models that
	// reject parameters their predecessors accepted (NENYA-32). Rules are
	// matched by model-ID prefix (config rules first, then the built-in
	// table; first match wins) and may drop parameters, clamp the
	// reasoning effort floor, or relax forced tool_choice.
	ParamCompat []ParamCompatRule `json:"param_compat,omitempty"`
	// AutoRetryOnParamReject enables the safety-net retry: when an
	// upstream 400 rejects a known parameter by name, the gateway strips
	// it and retries once (default false).
	AutoRetryOnParamReject *bool `json:"auto_retry_on_param_reject,omitempty"`
	// AllowedBrowserOrigins is the DNS-rebinding allowlist (NENYA-34):
	// requests carrying browser fetch metadata (Origin / Sec-Fetch-*) are
	// refused on every route — including the no-auth GET surfaces — unless
	// the request origin matches an entry. Default empty = deny all
	// browser-context requests; nenya serves non-browser clients.
	AllowedBrowserOrigins  []string `json:"allowed_browser_origins,omitempty"`
	CostMode               string   `json:"cost_mode,omitempty"`
	BillingEconomyScale    float64  `json:"billing_economy_scale,omitempty"`
	BillingQualityScale    float64  `json:"billing_quality_scale,omitempty"`
	MaxTransformedSSEBytes int      `json:"max_transformed_sse_bytes,omitempty"`
	UpstreamTimeoutSeconds *int     `json:"upstream_timeout_seconds,omitempty"`
	// StreamIdleTimeoutSeconds is the stall detection timeout for SSE streams.
	StreamIdleTimeoutSeconds *int `json:"stream_idle_timeout_seconds,omitempty"`
	// StreamBootstrapBufferBytes is the global stream bootstrap buffering
	// budget in bytes (NENYA-45): when >0, streaming responses hold
	// handshake events until a real output event (allow-list) or an
	// in-stream rejection is seen, keeping failover possible before the
	// 200 is committed. 0 (default) = off. Providers override via
	// providers.<name>.stream_bootstrap_buffer_bytes.
	StreamBootstrapBufferBytes int `json:"stream_bootstrap_buffer_bytes,omitempty"`
	// ThinkingStreamIdleTimeoutSeconds is the stall detection timeout during
	// thinking phases. When 0, disables thinking-aware extension.
	ThinkingStreamIdleTimeoutSeconds *int `json:"thinking_stream_idle_timeout_seconds,omitempty"`
	// StreamContinuation controls transparent continuation of upstream SSE
	// streams that end mid-generation without a [DONE] marker.
	StreamContinuation *StreamContinuationConfig `json:"stream_continuation,omitempty"`
}

func (g *GovernanceConfig) RPMSet() bool                { return wasSet(g.RatelimitMaxRPM) }
func (g *GovernanceConfig) TPMSet() bool                { return wasSet(g.RatelimitMaxTPM) }
func (g *GovernanceConfig) EmptyStreamAsErrorSet() bool { return wasSet(g.EmptyStreamAsError) }

// EarlyStreamErrorFailoverSet reports whether EarlyStreamErrorFailover was
// explicitly set in config (as opposed to defaulted by applyGovernanceDefaults).
func (g *GovernanceConfig) EarlyStreamErrorFailoverSet() bool {
	return wasSet(g.EarlyStreamErrorFailover)
}
func (g *GovernanceConfig) AutoContextSkipSet() bool      { return wasSet(g.AutoContextSkip) }
func (g *GovernanceConfig) AutoReorderByLatencySet() bool { return wasSet(g.AutoReorderByLatency) }

func (g *GovernanceConfig) EffectiveMaxRetryAttempts() int {
	if g.MaxRetryAttempts > 0 {
		return g.MaxRetryAttempts
	}
	return 3
}

// EffectiveMinQuotaCooldown returns the quota-class cooldown floor
// (NENYA-41): the configured value when positive, the built-in 10s default
// when zero or unset, and 0 (disabled) when negative. Capped at 24h like
// EffectiveUpstreamTimeout to keep operator typos from benching targets
// permanently (CWE-190: multiplication overflow guarded by the cap).
func (g *GovernanceConfig) EffectiveMinQuotaCooldown() time.Duration {
	const (
		defaultMinQuotaCooldown = 10 * time.Second
		maxMinQuotaCooldown     = 24 * time.Hour
	)
	switch {
	case g.MinQuotaCooldownSeconds < 0:
		return 0
	case g.MinQuotaCooldownSeconds == 0:
		return defaultMinQuotaCooldown
	default:
		d := time.Duration(g.MinQuotaCooldownSeconds) * time.Second
		if d < 0 || d > maxMinQuotaCooldown {
			return maxMinQuotaCooldown
		}
		return d
	}
}

func (g *GovernanceConfig) AutoRetryOnContextLimitEnabled() bool {
	return g.AutoRetryOnContextLimit != nil && *g.AutoRetryOnContextLimit
}

// ParamCompatRule declares parameter-compatibility sanitization for model
// IDs matching ModelPrefix (NENYA-32): parameters new model generations
// reject are dropped, the reasoning effort is clamped to a floor, and
// forced tool_choice can be relaxed to "auto".
type ParamCompatRule struct {
	ModelPrefix           string   `json:"model_prefix"`
	DropParams            []string `json:"drop_params,omitempty"`
	MinReasoningEffort    string   `json:"min_reasoning_effort,omitempty"`
	RelaxForcedToolChoice bool     `json:"relax_forced_tool_choice,omitempty"`
}

// AutoRetryOnParamRejectEnabled reports whether the strip-and-retry
// safety net for parameter rejections is enabled (NENYA-32).
func (g *GovernanceConfig) AutoRetryOnParamRejectEnabled() bool {
	return g.AutoRetryOnParamReject != nil && *g.AutoRetryOnParamReject
}

// EffectiveMaxTransformedSSEBytes returns the configured or default SSE
// transformed-output size limit. Defaults to 50MB when unset (≤0) and is
// capped at 1GB to prevent excessive memory allocation.
func (g *GovernanceConfig) EffectiveMaxTransformedSSEBytes() int {
	const (
		defaultLimit = 50 * 1024 * 1024
		maxLimit     = 1024 * 1024 * 1024
	)
	v := g.MaxTransformedSSEBytes
	if v <= 0 {
		return defaultLimit
	}
	if v > maxLimit {
		return maxLimit
	}
	return v
}

// EffectiveUpstreamTimeout returns the HTTP client timeout for upstream
// provider requests. When unset, defaults to 300s (5 minutes). When
// explicitly set to 0, returns 0 (no client-level deadline — relies on
// the stream idle timeout (default 300s, configurable via
// governance.stream_idle_timeout_seconds) for stall detection; connections
// without data flow will be killed). Negative values are rejected by
// validation at startup, but this method defensively clamps negatives to 0
// as well. Values exceeding 86400 are capped at 86400 (24h).
func (g *GovernanceConfig) EffectiveUpstreamTimeout() time.Duration {
	if g.UpstreamTimeoutSeconds == nil {
		return 300 * time.Second
	}
	v := *g.UpstreamTimeoutSeconds
	if v <= 0 {
		return 0
	}
	if v > 86400 {
		v = 86400
	}
	return time.Duration(v) * time.Second
}

// EffectiveStreamIdleTimeout returns the stall detection timeout for upstream
// SSE streams. When unset, defaults to 300s (5 minutes). When explicitly set
// to 0, returns 0 (disabled — no stall detection). Otherwise returns the
// configured value, capped at 86400 (24h). This timeout fires when NO data
// arrives from the upstream for the configured duration; it resets on every
// successful read. Models with long thinking phases (e.g. GLM-4.7) may have
// natural SSE gaps that require a higher value.
func (g *GovernanceConfig) EffectiveStreamIdleTimeout() time.Duration {
	if g.StreamIdleTimeoutSeconds == nil {
		return 300 * time.Second
	}
	v := *g.StreamIdleTimeoutSeconds
	if v <= 0 {
		return 0
	}
	if v > 86400 {
		v = 86400
	}
	return time.Duration(v) * time.Second
}

// EffectiveThinkingStreamIdleTimeout returns the stall detection timeout
// for thinking phases. When unset, defaults to 600s (10 minutes). When
// explicitly set to 0, returns 0 (disabled — no thinking-aware extension).
// Otherwise returns the configured value, capped at 86400 (24h). This is
// the extended timeout used when the SSETransformingReader detects active
// reasoning/thinking content in the stream.
func (g *GovernanceConfig) EffectiveThinkingStreamIdleTimeout() time.Duration {
	if g.ThinkingStreamIdleTimeoutSeconds == nil {
		return 600 * time.Second
	}
	v := *g.ThinkingStreamIdleTimeoutSeconds
	if v <= 0 {
		return 0
	}
	if v > 86400 {
		v = 86400
	}
	return time.Duration(v) * time.Second
}

// StreamContinuationConfig controls transparent continuation of upstream SSE
// streams. When enabled and an upstream stream ends mid-generation (content
// streamed but no finish_reason and no [DONE]), the gateway re-dispatches the
// same target with the partial assistant message appended so the client sees
// the stream keep flowing instead of a gateway_error. Recovery is skipped when
// a tool call is in flight (arguments are incomplete and cannot be resumed).
//
// All fields are optional; see applyGovernanceDefaults for defaults.
type StreamContinuationConfig struct {
	// Enabled toggles continuation. Defaults to true.
	Enabled *bool `json:"enabled,omitempty"`
	// MaxAttempts is the total number of stream attempts, including the
	// original. A value of 2 means one continuation retry. Defaults to 2 and
	// is capped at 5.
	MaxAttempts int `json:"max_attempts,omitempty"`
	// SameModelOnly restricts continuation to the same target/model. Defaults
	// to true. Cross-model fallback resume is unsupported; setting to false
	// opts out of continuation entirely.
	SameModelOnly *bool `json:"same_model_only,omitempty"`
	// IncludeReasoning appends partial reasoning_content to the assistant
	// message so reasoning-capable models resume with full context. Defaults to
	// false.
	IncludeReasoning bool `json:"include_reasoning,omitempty"`
}

// StreamContinuationEnabled reports whether stream continuation is enabled.
func (g *GovernanceConfig) StreamContinuationEnabled() bool {
	return g.StreamContinuation != nil &&
		g.StreamContinuation.Enabled != nil &&
		*g.StreamContinuation.Enabled
}

// EffectiveStreamContinuationMaxAttempts returns the maximum total stream
// attempts for continuation. Defaults to 2 (one continuation retry) when
// unset or non-positive, and is capped at 5 to bound amplification.
func (g *GovernanceConfig) EffectiveStreamContinuationMaxAttempts() int {
	const (
		defaultAttempts = 2
		maxAttempts     = 5
	)
	if g.StreamContinuation == nil || g.StreamContinuation.MaxAttempts <= 0 {
		return defaultAttempts
	}
	if g.StreamContinuation.MaxAttempts > maxAttempts {
		return maxAttempts
	}
	return g.StreamContinuation.MaxAttempts
}

// StreamContinuationIncludeReasoning reports whether partial reasoning
// content should be appended on continuation attempts.
func (g *GovernanceConfig) StreamContinuationIncludeReasoning() bool {
	return g.StreamContinuation != nil && g.StreamContinuation.IncludeReasoning
}

// StreamContinuationSameModelOnly reports whether continuation is restricted
// to the same target/model. Defaults to true when the section is absent or the
// field is unset. Continuation currently only re-dispatches the same target
// (cross-model fallback resume is unsupported), so an explicit false opts out
// entirely.
func (g *GovernanceConfig) StreamContinuationSameModelOnly() bool {
	return g.StreamContinuation == nil ||
		g.StreamContinuation.SameModelOnly == nil ||
		*g.StreamContinuation.SameModelOnly
}

// SecretsConfig holds sensitive credentials loaded from systemd credential
// files or /run/secrets/nenya. Not serialized in the main config file.
type SecretsConfig struct {
	ClientToken  string            `json:"client_token,omitempty"`
	ProviderKeys map[string]string `json:"provider_keys,omitempty"`
	ApiKeys      map[string]ApiKey `json:"api_keys,omitempty"`
}

// ApiKey defines an API key entry for client authentication, with
// associated roles, expiration, and fine-grained permissions.
type ApiKey struct {
	Name             string         `json:"name"`
	Token            string         `json:"token"`
	Roles            []string       `json:"roles"`
	AllowedAgents    []string       `json:"allowed_agents"`
	AllowedEndpoints []string       `json:"allowed_endpoints,omitempty"`
	CacheSalt        *string        `json:"cache_salt,omitempty"`
	CreatedAt        string         `json:"created_at,omitempty"`
	ExpiresAt        string         `json:"expires_at,omitempty"`
	Enabled          bool           `json:"enabled"`
	Permissions      map[string]any `json:"permissions,omitempty"`
	// RatelimitMaxRPM caps requests per minute for this key (0 =
	// unlimited). Enforced after authentication, before dispatch.
	RatelimitMaxRPM int `json:"ratelimit_max_rpm,omitempty"`
	// TokenBudgetDaily caps the token estimate this key can reserve per
	// UTC day (0 = unlimited). Estimates are non-refundable reservations.
	TokenBudgetDaily int `json:"token_budget_daily,omitempty"`
	// BudgetTier is the key's priority against provider daily budgets:
	// "always" (default) is served even when a provider's budget is
	// exhausted; "fill" is rejected while the provider budget has no
	// headroom. Only meaningful for providers that set token_budget_daily.
	BudgetTier string `json:"budget_tier,omitempty"`
}

func (k *ApiKey) Validate() error {
	if k.Token == "" {
		return errors.New("token cannot be empty")
	}
	if len(k.Token) < 16 {
		return errors.New("token too short (minimum 16 characters)")
	}
	if len(k.Token) > 512 {
		return errors.New("token too long (maximum 512 characters)")
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
	if k.CacheSalt != nil && len(*k.CacheSalt) > maxCacheSaltLength {
		return fmt.Errorf("cache_salt too long (maximum %d characters)", maxCacheSaltLength)
	}
	return nil
}

// maxCacheSaltLength caps the cache salt length to keep it hash-safe while
// allowing enough entropy for tenant isolation.
const maxCacheSaltLength = 64

const (
	// RoleAdmin grants full access to all agents and endpoints.
	RoleAdmin = "admin"
	// RoleUser grants access to configured agents.
	RoleUser = "user"
	// RoleReadOnly grants read-only access to non-mutating endpoints.
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

// EngineConfig defines a concrete engine endpoint for the bouncer
// summarization or window summarization pipeline: provider, model,
// prompt, and timeout.
type EngineConfig struct {
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	SystemPrompt     string `json:"system_prompt"`
	SystemPromptFile string `json:"system_prompt_file"`
	TimeoutSeconds   int    `json:"timeout_seconds"`
}

// EngineRef references an engine either by agent name (with fallback
// chain) or inline provider/model pair. After resolution via
// ResolveEngineRef, ResolvedTargets is populated with concrete EngineTargets.
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

// EngineTarget is a fully resolved engine endpoint containing both the
// EngineConfig (provider, model, prompts, timeout) and the resolved
// Provider (with URL and auth details).
type EngineTarget struct {
	Engine   EngineConfig
	Provider *Provider
}

// BouncerConfig controls the payload interception (bouncer) mechanism.
// When enabled, oversized messages are sent to a local Ollama engine
// for summarization and PII/credential redaction before forwarding upstream.
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

func (s *BouncerConfig) EnabledWasSet() bool  { return wasSet(s.Enabled) }
func (s *BouncerConfig) FailOpenWasSet() bool { return wasSet(s.FailOpen) }

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
	if len(aux.RedactPatterns) > 0 {
		s.RedactPatterns = aux.RedactPatterns
	}
	if aux.Enabled == nil && len(s.RedactPatterns) > 0 {
		s.Enabled = PtrTo(true)
	}
	return nil
}

// PrefixCacheConfig controls prompt prefix caching behavior, including
// pinning system prompts, stable tool definitions, and Anthropic cache_control.
// CacheMode selects the breakpoint strategy: CacheModeExplicit ("explicit",
// default) injects block-level markers on system/tools/messages, while
// CacheModeAutomatic ("automatic") emits a single top-level cache_control and
// disables block markers.
type PrefixCacheConfig struct {
	Enabled               bool    `json:"enabled"`
	CacheMode             *string `json:"cache_mode,omitempty"`
	PinSystemFirst        *bool   `json:"pin_system_first,omitempty"`
	StableTools           *bool   `json:"stable_tools,omitempty"`
	SkipRedactionOnSystem *bool   `json:"skip_redaction_on_system,omitempty"`
	CacheSystem           *bool   `json:"cache_system,omitempty"`
	CacheTools            *bool   `json:"cache_tools,omitempty"`
	CacheMessages         *bool   `json:"cache_messages,omitempty"`
	CacheControlTTL       string  `json:"cache_control_ttl,omitempty"`
	CacheSystemTTL        *string `json:"cache_system_ttl,omitempty"`
	CacheToolsTTL         *string `json:"cache_tools_ttl,omitempty"`
	CacheMessagesTTL      *string `json:"cache_messages_ttl,omitempty"`
	OpenAIBreakpoint      *bool   `json:"openai_breakpoint,omitempty"`
	OpenAIMode            *string `json:"openai_mode,omitempty"`
}

func (c *PrefixCacheConfig) CacheModeWasSet() bool        { return wasSet(c.CacheMode) }
func (c *PrefixCacheConfig) PinWasSet() bool              { return wasSet(c.PinSystemFirst) }
func (c *PrefixCacheConfig) StableWasSet() bool           { return wasSet(c.StableTools) }
func (c *PrefixCacheConfig) SkipRedactionWasSet() bool    { return wasSet(c.SkipRedactionOnSystem) }
func (c *PrefixCacheConfig) CacheSystemWasSet() bool      { return wasSet(c.CacheSystem) }
func (c *PrefixCacheConfig) CacheToolsWasSet() bool       { return wasSet(c.CacheTools) }
func (c *PrefixCacheConfig) CacheMessagesWasSet() bool    { return wasSet(c.CacheMessages) }
func (c *PrefixCacheConfig) CacheSystemTTLWasSet() bool   { return wasSet(c.CacheSystemTTL) }
func (c *PrefixCacheConfig) CacheToolsTTLWasSet() bool    { return wasSet(c.CacheToolsTTL) }
func (c *PrefixCacheConfig) CacheMessagesTTLWasSet() bool { return wasSet(c.CacheMessagesTTL) }
func (c *PrefixCacheConfig) OpenAIBreakpointWasSet() bool { return wasSet(c.OpenAIBreakpoint) }
func (c *PrefixCacheConfig) OpenAIModeWasSet() bool       { return wasSet(c.OpenAIMode) }

// CompactionPreset is a named preset for content compaction options.
type CompactionPreset string

const (
	// CompactionPresetAggressive enables all compaction features: JSON
	// minification, whitespace collapse, trailing whitespace trim, line
	// ending normalization, tool pruning, and thought pruning.
	CompactionPresetAggressive CompactionPreset = "aggressive"
	// CompactionPresetBalanced enables JSON minification, whitespace
	// collapse, trailing whitespace trim, and line ending normalization,
	// but not tool or thought pruning.
	CompactionPresetBalanced CompactionPreset = "balanced"
	// CompactionPresetMinimal disables all compaction features.
	CompactionPresetMinimal CompactionPreset = "minimal"
)

// CompactionConfig controls content compaction (minification, whitespace
// normalization, tool pruning) before forwarding payloads upstream.
type CompactionConfig struct {
	Preset                 CompactionPreset `json:"compaction_preset,omitempty"`
	Enabled                *bool            `json:"enabled,omitempty"`
	JSONMinify             *bool            `json:"json_minify,omitempty"`
	CollapseBlankLines     *bool            `json:"collapse_blank_lines,omitempty"`
	TrimTrailingWhitespace *bool            `json:"trim_trailing_whitespace,omitempty"`
	NormalizeLineEndings   *bool            `json:"normalize_line_endings,omitempty"`
	PruneStaleTools        *bool            `json:"prune_stale_tools,omitempty"`
	ToolProtectionWindow   int              `json:"tool_protection_window"`
	PruneThoughts          *bool            `json:"prune_thoughts,omitempty"`
}

func (c *CompactionConfig) EnabledWasSet() bool       { return wasSet(c.Enabled) }
func (c *CompactionConfig) MinifyWasSet() bool        { return wasSet(c.JSONMinify) }
func (c *CompactionConfig) CollapseWasSet() bool      { return wasSet(c.CollapseBlankLines) }
func (c *CompactionConfig) TrimWasSet() bool          { return wasSet(c.TrimTrailingWhitespace) }
func (c *CompactionConfig) NormWasSet() bool          { return wasSet(c.NormalizeLineEndings) }
func (c *CompactionConfig) PruneWasSet() bool         { return wasSet(c.PruneStaleTools) }
func (c *CompactionConfig) PruneThoughtsWasSet() bool { return wasSet(c.PruneThoughts) }

// WindowConfig controls the context window management strategy:
// truncation mode, active message count, trigger ratios, and the
// summarization engine to use.
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
	// SummaryRegenRatio is the regeneration hysteresis for summarize-mode
	// compaction (NENYA-24): a cached summary is reused — with only the
	// grown delta messages appended verbatim — until history grows by this
	// ratio since the summary was generated. Default 0.25.
	SummaryRegenRatio *float64 `json:"summary_regen_ratio,omitempty"`
	// SummaryCacheSize bounds the per-gateway summary cache (number of
	// conversation lineages). Default 512.
	SummaryCacheSize *int `json:"summary_cache_size,omitempty"`
}

// SummaryRegenRatioOrDefault returns the configured hysteresis ratio, or
// 0.25 when unset or out of range.
func (w WindowConfig) SummaryRegenRatioOrDefault() float64 {
	if w.SummaryRegenRatio != nil && *w.SummaryRegenRatio > 0 && *w.SummaryRegenRatio <= 1 {
		return *w.SummaryRegenRatio
	}
	return 0.25
}

// MCPServerConfig defines the connection parameters for an external MCP
// (Model Context Protocol) server: URL, optional headers, and timeout.
type MCPServerConfig struct {
	URL               string            `json:"url"`
	Headers           map[string]string `json:"headers,omitempty"`
	Timeout           int               `json:"timeout,omitempty"`
	KeepAliveInterval int               `json:"keep_alive_interval,omitempty"`
}

// AgentMCPConfig defines the MCP tool integration for an agent, listing
// the MCP servers to use, iteration limits, and auto-tool behavior.
type AgentMCPConfig struct {
	Servers       []string `json:"servers"`
	MaxIterations int      `json:"max_iterations,omitempty"`
	AutoSave      bool     `json:"auto_save,omitempty"`
	AutoSearch    bool     `json:"auto_search,omitempty"`
	SearchTool    string   `json:"search_tool,omitempty"`
	SaveTool      string   `json:"save_tool,omitempty"`
}

// DiscoveryConfig controls dynamic model discovery from upstream
// providers, including the optional auto-generated agent configs for
// discovered model categories (fast, reasoning, vision, etc.).
type DiscoveryConfig struct {
	Enabled          *bool             `json:"enabled,omitempty"`
	AutoAgents       *bool             `json:"auto_agents,omitempty"`
	AutoAgentsConfig *AutoAgentsConfig `json:"auto_agents_config,omitempty"`
}

// AutoAgentCategoryConfig enables or disables a specific auto-agent
// category (e.g. fast, reasoning, vision).
type AutoAgentCategoryConfig struct {
	Enabled bool `json:"enabled"`
}

// AutoAgentsConfig controls which categories of auto-generated agents
// are enabled during discovery. Each category maps to a pool of
// providers offering models in that category.
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

func (d *DiscoveryConfig) EnabledWasSet() bool    { return wasSet(d.Enabled) }
func (d *DiscoveryConfig) AutoAgentsWasSet() bool { return wasSet(d.AutoAgents) }

// ResponseCacheConfig controls the upstream response cache: max entries,
// entry size, TTL, eviction interval, and the force-refresh header name.
type ResponseCacheConfig struct {
	Enabled             *bool   `json:"enabled,omitempty"`
	MaxEntries          int     `json:"max_entries"`
	MaxEntryBytes       int64   `json:"max_entry_bytes"`
	TTLSeconds          int     `json:"ttl_seconds"`
	EvictEverySeconds   int     `json:"evict_every_seconds"`
	ForceRefreshHeader  string  `json:"force_refresh_header"`
	EnableSemantic      bool    `json:"enable_semantic,omitempty"`
	SimilarityThreshold float64 `json:"similarity_threshold,omitempty"`
	EmbeddingModel      string  `json:"embedding_model,omitempty"`
	EmbeddingURL        string  `json:"embedding_url,omitempty"`
}

func (c *ResponseCacheConfig) EnabledWasSet() bool { return wasSet(c.Enabled) }

// PtrTo returns a pointer to v. Used for ergonomic *bool/*int construction
// in config structs and test helpers. The zero value (nil) represents
// "not set" vs an explicit false/zero value.
func PtrTo[T any](v T) *T { return &v }
