package testutil

import (
	"context"
	"net/http"
	"testing"
	"time"

	"nenya/internal/config"
)

// DefaultConfig returns a minimal valid Config for testing.
// It uses in-memory defaults and safe values.
func DefaultConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			ListenAddr:   ":0",
			MaxBodyBytes: 10 * 1024 * 1024,
			UserAgent:    "Nenya-Test/1.0",
		},
		Governance: config.GovernanceConfig{
			BlockedExecutionPatterns: []string{},
			RatelimitMaxRPM:           1000,
			RatelimitMaxTPM:           100000,
			TruncationStrategy:        "keep_first_last",
			KeepFirstPercent:          0.2,
			KeepLastPercent:           0.8,
			RetryableStatusCodes:      []int{429, 500, 502, 503, 504},
		},
		SecurityFilter: config.SecurityFilterConfig{
			Enabled:             false,
			RedactionLabel:      "[REDACTED]",
			Patterns:            []string{},
			OutputEnabled:       false,
			OutputWindowChars:   1000,
			SkipOnEngineFailure: true,
			EntropyEnabled:      false,
			EntropyThreshold:    3.5,
			EntropyMinToken:     10,
		},
		PrefixCache: config.PrefixCacheConfig{
			Enabled:               false,
			PinSystemFirst:        false,
			StableTools:           false,
			SkipRedactionOnSystem: false,
		},
		Compaction: config.CompactionConfig{
			Enabled:                false,
			JSONMinify:             false,
			CollapseBlankLines:     false,
			TrimTrailingWhitespace: false,
			NormalizeLineEndings:   false,
			PruneStaleTools:        false,
			ToolProtectionWindow:   60,
			PruneThoughts:          false,
		},
		Window: config.WindowConfig{
			Enabled:         false,
			Mode:            "summary",
			ActiveMessages:  10,
			TriggerRatio:    0.8,
			SummaryMaxRunes: 2000,
			MaxContext:      100000,
		},
		ResponseCache: config.ResponseCacheConfig{
			Enabled: false,
		},
		MCPServers: map[string]config.MCPServerConfig{},
		Agents:     map[string]config.AgentConfig{},
		Providers:  map[string]config.ProviderConfig{},
	}
}

// ConfigOption is a functional option for modifying a test Config.
type ConfigOption func(*config.Config)

// WithListenAddr sets the server listen address.
func WithListenAddr(addr string) ConfigOption {
	return func(c *config.Config) {
		c.Server.ListenAddr = addr
	}
}

// WithMaxBodyBytes sets the maximum request body size.
func WithMaxBodyBytes(bytes int64) ConfigOption {
	return func(c *config.Config) {
		c.Server.MaxBodyBytes = bytes
	}
}

// WithUserAgent sets the user agent string.
func WithUserAgent(ua string) ConfigOption {
	return func(c *config.Config) {
		c.Server.UserAgent = ua
	}
}

// WithGovernance sets the governance config.
func WithGovernance(g config.GovernanceConfig) ConfigOption {
	return func(c *config.Config) {
		c.Governance = g
	}
}

// WithRatelimit sets the rate limits.
func WithRatelimit(rpm, tpm int) ConfigOption {
	return func(c *config.Config) {
		c.Governance.RatelimitMaxRPM = rpm
		c.Governance.RatelimitMaxTPM = tpm
	}
}

// WithTruncationStrategy sets the truncation strategy and percentages.
func WithTruncationStrategy(strategy string, first, last float64) ConfigOption {
	return func(c *config.Config) {
		c.Governance.TruncationStrategy = strategy
		c.Governance.KeepFirstPercent = first
		c.Governance.KeepLastPercent = last
	}
}

// WithSecurityFilter sets the security filter config.
func WithSecurityFilter(s config.SecurityFilterConfig) ConfigOption {
	return func(c *config.Config) {
		c.SecurityFilter = s
	}
}

// WithSecurityFilterEnabled enables the security filter with basic settings.
func WithSecurityFilterEnabled(patterns []string) ConfigOption {
	return func(c *config.Config) {
		c.SecurityFilter.Enabled = true
		c.SecurityFilter.Patterns = patterns
	}
}

// WithPrefixCache sets the prefix cache config.
func WithPrefixCache(p config.PrefixCacheConfig) ConfigOption {
	return func(c *config.Config) {
		c.PrefixCache = p
	}
}

// WithCompaction sets the compaction config.
func WithCompaction(c2 config.CompactionConfig) ConfigOption {
	return func(c *config.Config) {
		c.Compaction = c2
	}
}

// WithWindow sets the window config.
func WithWindow(w config.WindowConfig) ConfigOption {
	return func(c *config.Config) {
		c.Window = w
	}
}

// WithResponseCache sets the response cache config.
func WithResponseCache(r config.ResponseCacheConfig) ConfigOption {
	return func(c *config.Config) {
		c.ResponseCache = r
	}
}

// WithMCPServer adds or replaces an MCP server config.
func WithMCPServer(name string, s config.MCPServerConfig) ConfigOption {
	return func(c *config.Config) {
		if c.MCPServers == nil {
			c.MCPServers = make(map[string]config.MCPServerConfig)
		}
		c.MCPServers[name] = s
	}
}

// WithProvider adds or replaces a provider config.
func WithProvider(name string, p config.ProviderConfig) ConfigOption {
	return func(c *config.Config) {
		if c.Providers == nil {
			c.Providers = make(map[string]config.ProviderConfig)
		}
		c.Providers[name] = p
	}
}

// WithAgent adds or replaces an agent config.
func WithAgent(name string, a config.AgentConfig) ConfigOption {
	return func(c *config.Config) {
		if c.Agents == nil {
			c.Agents = make(map[string]config.AgentConfig)
		}
		c.Agents[name] = a
	}
}

// TestConfig returns a Config with the given options applied.
func TestConfig(opts ...ConfigOption) *config.Config {
	c := DefaultConfig()
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// LoadTestConfig loads a config from a testdata file and applies options.
// If the file does not exist, it returns DefaultConfig with options.
func LoadTestConfig(tb testing.TB, path string, opts ...ConfigOption) *config.Config {
	tb.Helper()

	cfg, err := config.Load(path)
	if err != nil {
		tb.Fatalf("failed to load test config: %v", err)
	}

	for _, opt := range opts {
		opt(cfg)
	}

	return cfg
}

// NewTestHTTPClient returns an http.Client with safe defaults for testing.
func NewTestHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:       10,
			IdleConnTimeout:    5 * time.Second,
			DisableCompression: false,
			DisableKeepAlives:  false,
		},
	}
}

// NewTestContext returns a context with a short timeout for testing.
func NewTestContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}
