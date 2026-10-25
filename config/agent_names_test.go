package config

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// TestValidateAgentNamesRejectsColons pins the AUD-40 fix (NENYA-238): agent
// names must not contain a colon. The circuit key is
// `agent:provider:model`, and both consumers that parse it positionally —
// the per-agent breaker threshold lookup (first colon-delimited field) and
// the backoff metric labels (SplitN 3) — silently misparse an agent whose
// name carries a colon: thresholds fell back to the defaults and metric
// labels were scrambled. The key format cannot represent such names, so
// load-time validation rejects them with a clear message instead of the
// silent corruption.
func TestValidateAgentNamesRejectsColons(t *testing.T) {
	cases := []struct {
		name    string
		agents  map[string]AgentConfig
		wantErr []string // substrings; empty = no errors
	}{
		{
			name:   "plain names pass",
			agents: map[string]AgentConfig{"agent-1": {}, "my_agent.2": {}},
		},
		{
			name:    "colon in name rejected",
			agents:  map[string]AgentConfig{"my:agent": {}},
			wantErr: []string{`agents["my:agent"]`, "colon"},
		},
		{
			name:    "empty name rejected",
			agents:  map[string]AgentConfig{"": {}},
			wantErr: []string{"agent name must not be empty"},
		},
		{
			name: "colon elsewhere is fine",
			agents: map[string]AgentConfig{
				"ok": {Models: []AgentModel{{Provider: "p", Model: "gpt-4o:latest"}}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Drive the COLLECTION point, not validateAgentNames directly:
			// the wiring into the validation pipeline is part of the fix —
			// an unwired validator is exactly the AUD-44 failure mode
			// (a sweeper that exists but is never called).
			cfg := &Config{Agents: tc.agents}
			collected := ValidateConfigReferences(cfg, &SecretsConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if len(tc.wantErr) == 0 {
				if len(collected) != 0 {
					t.Fatalf("expected no errors, got %v", collected)
				}
				return
			}
			if len(collected) == 0 {
				t.Fatalf("expected an error, got none")
			}
			joined := strings.Join(collected, "\n")
			for _, want := range tc.wantErr {
				if !strings.Contains(joined, want) {
					t.Fatalf("expected an error containing %q, got:\n%s", want, joined)
				}
			}
		})
	}
}
