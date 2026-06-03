package testutil

import (
	"git.0ur.uk/nenya/config"
)

// MinimalConfig returns a minimal config with only required fields set.
// Useful for tests that don't need specific features enabled.
func MinimalConfig() *config.Config {
	rpm := 1000
	tpm := 100000
	falseVal := false
	return &config.Config{
		Server: config.ServerConfig{
			ListenAddr:   ":0",
			MaxBodyBytes: 10 * 1024 * 1024,
			UserAgent:    "Nenya-Test/1.0",
		},
		Context: config.ContextConfig{
			TruncationStrategy:     "keep_first_last",
			TruncationKeepFirstPct: 0.2,
			TruncationKeepLastPct:  0.8,
		},
		Governance: config.GovernanceConfig{
			RatelimitMaxRPM:      &rpm,
			RatelimitMaxTPM:      &tpm,
			RetryableStatusCodes: []int{429, 500, 502, 503, 504},
		},
		Bouncer: config.BouncerConfig{
			Enabled:        &falseVal,
			RedactionLabel: "[REDACTED]",
			RedactOutput:   false,
			FailOpen:       config.PtrTo(true),
		},
		PrefixCache: config.PrefixCacheConfig{
			Enabled: false,
		},
		Compaction: config.CompactionConfig{
			Enabled: &falseVal,
		},
		Window: config.WindowConfig{
			Enabled: false,
		},
		ResponseCache: config.ResponseCacheConfig{
			Enabled: &falseVal,
		},
		MCPServers: map[string]config.MCPServerConfig{},
		Agents:     map[string]config.AgentConfig{},
		Providers:  map[string]config.ProviderConfig{},
	}
}
