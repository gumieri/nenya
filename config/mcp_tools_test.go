package config

import (
	"strings"
	"testing"
)

// validateMCPToolSurfaces (NENYA-162) rejects the misconfigurations that would
// otherwise silently misbehave: a negative tool_search, a floor without
// selection enabled, a glob floor (matched by equality), and empty or
// control-character include/exclude patterns.
func TestValidateMCPToolSurfaces(t *testing.T) {
	agent := func(mcp *AgentMCPConfig) *Config {
		return &Config{Agents: map[string]AgentConfig{"a": {MCP: mcp}}}
	}

	cases := []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{
			name: "valid selection with an exact floor",
			cfg:  agent(&AgentMCPConfig{ToolSearch: 5, ToolSearchFloor: []string{"save_note"}}),
		},
		{
			name:    "negative tool_search",
			cfg:     agent(&AgentMCPConfig{ToolSearch: -1}),
			wantErr: "must not be negative",
		},
		{
			name:    "floor without selection",
			cfg:     agent(&AgentMCPConfig{ToolSearchFloor: []string{"save_note"}}),
			wantErr: "tool_search is off",
		},
		{
			name:    "glob floor",
			cfg:     agent(&AgentMCPConfig{ToolSearch: 3, ToolSearchFloor: []string{"save_*"}}),
			wantErr: "without glob metacharacters",
		},
		{
			name:    "empty floor entry",
			cfg:     agent(&AgentMCPConfig{ToolSearch: 3, ToolSearchFloor: []string{""}}),
			wantErr: "without glob metacharacters",
		},
		{
			name: "valid include/exclude globs",
			cfg: &Config{MCPServers: map[string]MCPServerConfig{
				"s": {ToolInclude: []string{"get_*"}, ToolExclude: []string{"delete_?"}},
			}},
		},
		{
			name: "empty include pattern",
			cfg: &Config{MCPServers: map[string]MCPServerConfig{
				"s": {ToolInclude: []string{""}},
			}},
			wantErr: "non-empty name/glob",
		},
		{
			name: "control character in a pattern",
			cfg: &Config{MCPServers: map[string]MCPServerConfig{
				"s": {ToolExclude: []string{"bad\nname"}},
			}},
			wantErr: "non-empty name/glob",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(validateMCPToolSurfaces(tc.cfg), "; ")
			if tc.wantErr == "" {
				if got != "" {
					t.Fatalf("unexpected errors: %s", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantErr) {
				t.Fatalf("want an error containing %q, got %q", tc.wantErr, got)
			}
		})
	}
}
