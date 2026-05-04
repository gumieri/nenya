package testutil

import (
	"testing"

	"nenya/config"
)

// DefaultConfig returns a minimal valid Config for testing.
// It uses in-memory defaults and safe values.
// Deprecated: Use MinimalConfig() instead.
func DefaultConfig() *config.Config {
	return MinimalConfig()
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
		c.Governance.RatelimitMaxRPM = config.PtrTo(rpm)
		c.Governance.RatelimitMaxTPM = config.PtrTo(tpm)
	}
}

// WithTruncationStrategy sets the truncation strategy and percentages.
func WithTruncationStrategy(strategy string, first, last float64) ConfigOption {
	return func(c *config.Config) {
		c.Context.TruncationStrategy = strategy
		c.Context.TruncationKeepFirstPct = first
		c.Context.TruncationKeepLastPct = last
	}
}

// WithSecurityFilter sets the security filter config.
func WithSecurityFilter(s config.BouncerConfig) ConfigOption {
	return func(c *config.Config) {
		c.Bouncer = s
	}
}

// WithSecurityFilterEnabled enables the security filter with basic settings.
func WithSecurityFilterEnabled(patterns []string) ConfigOption {
	return func(c *config.Config) {
		c.Bouncer.Enabled = config.PtrTo(true)
		c.Bouncer.RedactPatterns = patterns
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
	c := MinimalConfig()
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// LoadTestConfig loads a config from a testdata file and applies options.
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
