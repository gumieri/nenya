package testutil

import (
	"nenya/internal/config"
)

// MinimalConfig returns a minimal config with only required fields set.
// Useful for tests that don't need specific features enabled.
func MinimalConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			ListenAddr:   ":0",
			MaxBodyBytes: 10 * 1024 * 1024,
			UserAgent:    "Nenya-Test/1.0",
		},
		Governance: config.GovernanceConfig{
			RatelimitMaxRPM:      1000,
			RatelimitMaxTPM:      100000,
			TruncationStrategy:   "keep_first_last",
			KeepFirstPercent:     0.2,
			KeepLastPercent:      0.8,
			RetryableStatusCodes: []int{429, 500, 502, 503, 504},
		},
		SecurityFilter: config.SecurityFilterConfig{
			Enabled:             false,
			RedactionLabel:      "[REDACTED]",
			OutputEnabled:       false,
			SkipOnEngineFailure: true,
		},
		PrefixCache: config.PrefixCacheConfig{
			Enabled: false,
		},
		Compaction: config.CompactionConfig{
			Enabled: false,
		},
		Window: config.WindowConfig{
			Enabled: false,
		},
		ResponseCache: config.ResponseCacheConfig{
			Enabled: false,
		},
		MCPServers: map[string]config.MCPServerConfig{},
		Agents:     map[string]config.AgentConfig{},
		Providers:  map[string]config.ProviderConfig{},
	}
}

// SecurityFilterConfig returns a config with security filter enabled.
// Useful for testing PII redaction and content filtering.
func SecurityFilterConfig() *config.Config {
	cfg := MinimalConfig()
	cfg.SecurityFilter.Enabled = true
	cfg.SecurityFilter.Patterns = []string{
		`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Z|a-z]{2,}\b`,
		`\b\d{3}-\d{2}-\d{4}\b`,
	}
	cfg.SecurityFilter.OutputEnabled = true
	cfg.SecurityFilter.OutputWindowChars = 1000
	cfg.SecurityFilter.Engine = config.EngineRef{
		Provider: "ollama",
		Model:    "qwen2.5-coder",
	}
	return cfg
}

// CompactionConfig returns a config with compaction enabled.
// Useful for testing message compaction and tool pruning.
func CompactionConfig() *config.Config {
	cfg := MinimalConfig()
	cfg.Compaction.Enabled = true
	cfg.Compaction.JSONMinify = true
	cfg.Compaction.CollapseBlankLines = true
	cfg.Compaction.TrimTrailingWhitespace = true
	cfg.Compaction.NormalizeLineEndings = true
	cfg.Compaction.PruneStaleTools = true
	cfg.Compaction.ToolProtectionWindow = 60
	cfg.Compaction.PruneThoughts = true
	return cfg
}

// WindowConfig returns a config with windowing enabled.
// Useful for testing context window management.
func WindowConfig() *config.Config {
	cfg := MinimalConfig()
	cfg.Window.Enabled = true
	cfg.Window.Mode = "summary"
	cfg.Window.ActiveMessages = 10
	cfg.Window.TriggerRatio = 0.8
	cfg.Window.SummaryMaxRunes = 2000
	cfg.Window.MaxContext = 100000
	cfg.Window.Engine = config.EngineRef{
		Provider: "ollama",
		Model:    "qwen2.5-coder",
	}
	return cfg
}

// PrefixCacheConfig returns a config with prefix cache enabled.
// Useful for testing system prompt caching.
func PrefixCacheConfig() *config.Config {
	cfg := MinimalConfig()
	cfg.PrefixCache.Enabled = true
	cfg.PrefixCache.PinSystemFirst = true
	cfg.PrefixCache.StableTools = true
	cfg.PrefixCache.SkipRedactionOnSystem = true
	return cfg
}

// ResponseCacheConfig returns a config with response cache enabled.
// Useful for testing response caching.
func ResponseCacheConfig() *config.Config {
	cfg := MinimalConfig()
	cfg.ResponseCache.Enabled = true
	return cfg
}

// MCPConfig returns a config with an MCP server configured.
// Useful for testing MCP tool integration.
func MCPConfig() *config.Config {
	cfg := MinimalConfig()
	cfg.MCPServers = map[string]config.MCPServerConfig{
		"test-server": {
			URL:               "http://localhost:3000",
			Headers:           map[string]string{"Authorization": "Bearer test-token"},
			Timeout:           30,
			KeepAliveInterval: 60,
		},
	}
	return cfg
}

// AgentConfig returns a config with a test agent configured.
// Useful for testing agent routing and fallback chains.
func AgentConfig() *config.Config {
	cfg := MinimalConfig()
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy:         "fallback",
			CooldownSeconds:  60,
			FailureThreshold: 5,
			FailureWindowSec: 300,
			SuccessThreshold: 2,
			MaxRetries:       3,
			Models: []config.AgentModel{
				{
					Provider:   "openai",
					Model:      "gpt-4",
					MaxContext: 128000,
					MaxOutput:  4096,
				},
			},
		},
	}
	return cfg
}

// ProviderConfig returns a config with a test provider configured.
// Useful for testing provider routing.
func ProviderConfig() *config.Config {
	cfg := MinimalConfig()
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider": {
			URL:                  "https://api.example.com/v1",
			RoutePrefixes:        []string{"test-"},
			AuthStyle:            "bearer",
			ApiFormat:            "openai",
			TimeoutSeconds:       30,
			RetryableStatusCodes: []int{429, 500, 502, 503, 504},
		},
	}
	return cfg
}

// FullConfig returns a config with all features enabled.
// Useful for integration tests that need the full gateway.
func FullConfig() *config.Config {
	cfg := MinimalConfig()
	cfg.SecurityFilter.Enabled = true
	cfg.SecurityFilter.Patterns = []string{
		`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Z|a-z]{2,}\b`,
	}
	cfg.SecurityFilter.Engine = config.EngineRef{
		Provider: "ollama",
		Model:    "qwen2.5-coder",
	}
	cfg.Compaction.Enabled = true
	cfg.Compaction.JSONMinify = true
	cfg.Compaction.CollapseBlankLines = true
	cfg.Compaction.TrimTrailingWhitespace = true
	cfg.Compaction.NormalizeLineEndings = true
	cfg.Compaction.PruneStaleTools = true
	cfg.Compaction.ToolProtectionWindow = 60
	cfg.Compaction.PruneThoughts = true
	cfg.Window.Enabled = true
	cfg.Window.Mode = "summary"
	cfg.Window.ActiveMessages = 10
	cfg.Window.TriggerRatio = 0.8
	cfg.Window.SummaryMaxRunes = 2000
	cfg.Window.MaxContext = 100000
	cfg.Window.Engine = config.EngineRef{
		Provider: "ollama",
		Model:    "qwen2.5-coder",
	}
	cfg.PrefixCache.Enabled = true
	cfg.PrefixCache.PinSystemFirst = true
	cfg.PrefixCache.StableTools = true
	cfg.PrefixCache.SkipRedactionOnSystem = true
	cfg.ResponseCache.Enabled = true
	cfg.MCPServers = map[string]config.MCPServerConfig{
		"test-server": {
			URL:               "http://localhost:3000",
			Headers:           map[string]string{"Authorization": "Bearer test-token"},
			Timeout:           30,
			KeepAliveInterval: 60,
		},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy:         "fallback",
			CooldownSeconds:  60,
			FailureThreshold: 5,
			FailureWindowSec: 300,
			SuccessThreshold: 2,
			MaxRetries:       3,
			Models: []config.AgentModel{
				{
					Provider:   "openai",
					Model:      "gpt-4",
					MaxContext: 128000,
					MaxOutput:  4096,
				},
			},
		},
	}
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider": {
			URL:                  "https://api.example.com/v1",
			RoutePrefixes:        []string{"test-"},
			AuthStyle:            "bearer",
			ApiFormat:            "openai",
			TimeoutSeconds:       30,
			RetryableStatusCodes: []int{429, 500, 502, 503, 504},
		},
	}
	return cfg
}
