package config

import (
	"strings"
	"testing"
	"time"
)

// NENYA-153 — slow-call threshold configuration: resolution (per-agent override
// over global) and the boot-time validation rules.

func TestSlowCallThresholdResolution(t *testing.T) {
	tests := []struct {
		name   string
		global *int
		agent  *int
		want   time.Duration
	}{
		{name: "both unset is off", want: 0},
		{name: "global applies", global: PtrTo(5), want: 5 * time.Second},
		{name: "agent overrides global", global: PtrTo(5), agent: PtrTo(2), want: 2 * time.Second},
		{name: "agent zero disables the global", global: PtrTo(5), agent: PtrTo(0), want: 0},
		{name: "negative global is off", global: PtrTo(-1), want: 0},
		{name: "negative agent disables", global: PtrTo(5), agent: PtrTo(-1), want: 0},
		{name: "global is capped at 24h", global: PtrTo(100000), want: 86400 * time.Second},
		{name: "agent is capped at 24h", agent: PtrTo(100000), want: 86400 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			global := GovernanceConfig{SlowCallThresholdSeconds: tt.global}
			agent := AgentConfig{SlowCallThresholdSeconds: tt.agent}
			if got := agent.EffectiveSlowCallThreshold(global.EffectiveSlowCallThreshold()); got != tt.want {
				t.Fatalf("EffectiveSlowCallThreshold() = %v, want %v", got, tt.want)
			}
		})
	}

	var nilGovernance *GovernanceConfig
	if nilGovernance.EffectiveSlowCallThreshold() != 0 {
		t.Error("a nil governance config must resolve to off")
	}
	var nilAgent *AgentConfig
	if nilAgent.EffectiveSlowCallThreshold(3*time.Second) != 3*time.Second {
		t.Error("a nil agent config must inherit the global threshold")
	}
}

// TestSlowCallThresholdValidation pins NENYA-153's boot-time rules: a negative
// threshold is a configuration mistake (0 is how the feature is disabled) and an
// over-long one is rejected rather than silently capped.
func TestSlowCallThresholdValidation(t *testing.T) {
	tests := []struct {
		name    string
		value   *int
		wantErr bool
	}{
		{name: "unset is valid", value: nil},
		{name: "zero disables", value: PtrTo(0)},
		{name: "positive is valid", value: PtrTo(30)},
		{name: "24h is valid", value: PtrTo(86400)},
		{name: "negative is rejected", value: PtrTo(-1), wantErr: true},
		{name: "above 24h is rejected", value: PtrTo(86401), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			globalErrs := validateSlowCallThresholdSeconds(tt.value, "governance.slow_call_threshold_seconds")
			agentErrs := validateAgentSlowCallThresholds(map[string]AgentConfig{
				"a": {SlowCallThresholdSeconds: tt.value},
			})
			if gotErr := len(globalErrs) > 0; gotErr != tt.wantErr {
				t.Fatalf("global validation error = %v (%v), want error = %v", gotErr, globalErrs, tt.wantErr)
			}
			if gotErr := len(agentErrs) > 0; gotErr != tt.wantErr {
				t.Fatalf("agent validation error = %v (%v), want error = %v", gotErr, agentErrs, tt.wantErr)
			}
			if tt.wantErr {
				if !strings.Contains(globalErrs[0], "governance.slow_call_threshold_seconds") {
					t.Fatalf("global error does not name the field: %v", globalErrs)
				}
				if !strings.Contains(agentErrs[0], `agents["a"].slow_call_threshold_seconds`) {
					t.Fatalf("agent error does not name the field: %v", agentErrs)
				}
			}
		})
	}
}
