package testutil

import (
	"git.0ur.uk/nenya/config"
)

// ConfigOption is a functional option for modifying a test Config.
type ConfigOption func(*config.Config)

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
