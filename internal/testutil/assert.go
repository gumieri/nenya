package testutil

import (
	"reflect"
	"testing"

	"nenya/internal/config"
)

// AssertConfigEqual asserts that two configs are equal.
// Useful for verifying config transformations.
func AssertConfigEqual(t *testing.T, expected, actual *config.Config) {
	t.Helper()

	if !reflect.DeepEqual(expected, actual) {
		t.Errorf("configs not equal:\nexpected: %+v\nactual:   %+v", expected, actual)
	}
}

// AssertConfigFieldEqual asserts that a specific field in two configs is equal.
// Useful for verifying specific config changes.
func AssertConfigFieldEqual(t *testing.T, field string, expected, actual interface{}) {
	t.Helper()

	if !reflect.DeepEqual(expected, actual) {
		t.Errorf("config field %s not equal:\nexpected: %+v\nactual:   %+v", field, expected, actual)
	}
}

// AssertConfigHasProvider asserts that a config has a specific provider.
func AssertConfigHasProvider(t *testing.T, cfg *config.Config, providerName string) {
	t.Helper()

	if cfg.Providers == nil {
		t.Fatalf("config has no providers map")
	}

	if _, ok := cfg.Providers[providerName]; !ok {
		t.Errorf("config does not have provider %q", providerName)
	}
}

// AssertConfigHasAgent asserts that a config has a specific agent.
func AssertConfigHasAgent(t *testing.T, cfg *config.Config, agentName string) {
	t.Helper()

	if cfg.Agents == nil {
		t.Fatalf("config has no agents map")
	}

	if _, ok := cfg.Agents[agentName]; !ok {
		t.Errorf("config does not have agent %q", agentName)
	}
}

// AssertConfigHasMCPServer asserts that a config has a specific MCP server.
func AssertConfigHasMCPServer(t *testing.T, cfg *config.Config, serverName string) {
	t.Helper()

	if cfg.MCPServers == nil {
		t.Fatalf("config has no MCP servers map")
	}

	if _, ok := cfg.MCPServers[serverName]; !ok {
		t.Errorf("config does not have MCP server %q", serverName)
	}
}

// AssertSecurityFilterEnabled asserts that security filter is enabled.
func AssertSecurityFilterEnabled(t *testing.T, cfg *config.Config) {
	t.Helper()

	if !cfg.SecurityFilter.Enabled {
		t.Errorf("expected security filter to be enabled")
	}
}

// AssertSecurityFilterDisabled asserts that security filter is disabled.
func AssertSecurityFilterDisabled(t *testing.T, cfg *config.Config) {
	t.Helper()

	if cfg.SecurityFilter.Enabled {
		t.Errorf("expected security filter to be disabled")
	}
}

// AssertCompactionEnabled asserts that compaction is enabled.
func AssertCompactionEnabled(t *testing.T, cfg *config.Config) {
	t.Helper()

	if !cfg.Compaction.Enabled {
		t.Errorf("expected compaction to be enabled")
	}
}

// AssertWindowEnabled asserts that windowing is enabled.
func AssertWindowEnabled(t *testing.T, cfg *config.Config) {
	t.Helper()

	if !cfg.Window.Enabled {
		t.Errorf("expected windowing to be enabled")
	}
}

// AssertPrefixCacheEnabled asserts that prefix cache is enabled.
func AssertPrefixCacheEnabled(t *testing.T, cfg *config.Config) {
	t.Helper()

	if !cfg.PrefixCache.Enabled {
		t.Errorf("expected prefix cache to be enabled")
	}
}

// AssertResponseCacheEnabled asserts that response cache is enabled.
func AssertResponseCacheEnabled(t *testing.T, cfg *config.Config) {
	t.Helper()

	if !cfg.ResponseCache.Enabled {
		t.Errorf("expected response cache to be enabled")
	}
}

// AssertRatelimitSet asserts that rate limits are configured.
func AssertRatelimitSet(t *testing.T, cfg *config.Config, rpm, tpm int) {
	t.Helper()

	if cfg.Governance.RatelimitMaxRPM != rpm {
		t.Errorf("expected RPM limit %d, got %d", rpm, cfg.Governance.RatelimitMaxRPM)
	}

	if cfg.Governance.RatelimitMaxTPM != tpm {
		t.Errorf("expected TPM limit %d, got %d", tpm, cfg.Governance.RatelimitMaxTPM)
	}
}

// AssertMaxBodyBytes asserts that max body bytes is set to expected value.
func AssertMaxBodyBytes(t *testing.T, cfg *config.Config, expected int64) {
	t.Helper()

	if cfg.Server.MaxBodyBytes != expected {
		t.Errorf("expected MaxBodyBytes %d, got %d", expected, cfg.Server.MaxBodyBytes)
	}
}

// AssertListenAddr asserts that listen address is set to expected value.
func AssertListenAddr(t *testing.T, cfg *config.Config, expected string) {
	t.Helper()

	if cfg.Server.ListenAddr != expected {
		t.Errorf("expected ListenAddr %q, got %q", expected, cfg.Server.ListenAddr)
	}
}
