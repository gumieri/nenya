package routing

import (
	"testing"

	"github.com/nenya/config"
)

func TestResolveWindowMaxOutput_WithModels(t *testing.T) {
	agents := map[string]config.AgentConfig{
		"test-agent": {
			Models: []config.AgentModel{
				{Provider: "deepseek", Model: "deepseek-v4-flash"},
				{Provider: "gemini", Model: "gemini-2.5-flash"},
			},
		},
	}
	got := ResolveWindowMaxOutput("test-agent", agents, nil)
	if got != 384000 {
		t.Fatalf("expected 384000 (max of 384000 and 65536), got %d", got)
	}
}

func TestResolveWindowMaxOutput_AgentNotFound(t *testing.T) {
	agents := map[string]config.AgentConfig{}
	got := ResolveWindowMaxOutput("nonexistent", agents, nil)
	if got != 0 {
		t.Fatalf("expected 0 for nonexistent agent, got %d", got)
	}
}

func TestResolveAgentCapabilities_Intersection(t *testing.T) {
	tests := []struct {
		name          string
		agentName     string
		agents        map[string]config.AgentConfig
		wantVision    bool
		wantToolCalls bool
		wantReasoning bool
	}{
		{
			name:      "all models support all capabilities",
			agentName: "agent-all",
			agents: map[string]config.AgentConfig{
				"agent-all": {
					Models: []config.AgentModel{
						{Provider: "gemini", Model: "gemini-2.5-flash"},
						{Provider: "gemini", Model: "gemini-2.5-pro"},
					},
				},
			},
			wantVision:    true,
			wantToolCalls: true,
			wantReasoning: true,
		},
		{
			name:      "one model lacks vision (nemotron has none)",
			agentName: "agent-mixed",
			agents: map[string]config.AgentConfig{
				"agent-mixed": {
					Models: []config.AgentModel{
						{Provider: "gemini", Model: "gemini-2.5-flash"},
						{Provider: "nvidia_free", Model: "nemotron-3-super"},
					},
				},
			},
			wantVision:    false,
			wantToolCalls: false,
			wantReasoning: false,
		},
		{
			name:      "empty agent",
			agentName: "agent-empty",
			agents: map[string]config.AgentConfig{
				"agent-empty": {Models: nil},
			},
			wantVision:    false,
			wantToolCalls: false,
			wantReasoning: false,
		},
		{
			name:       "agent not found",
			agents:     map[string]config.AgentConfig{},
			wantVision: false, wantToolCalls: false, wantReasoning: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveAgentCapabilities(tt.agentName, tt.agents, nil)
			if got.SupportsVision != tt.wantVision {
				t.Errorf("SupportsVision = %v, want %v", got.SupportsVision, tt.wantVision)
			}
			if got.SupportsToolCalls != tt.wantToolCalls {
				t.Errorf("SupportsToolCalls = %v, want %v", got.SupportsToolCalls, tt.wantToolCalls)
			}
			if got.SupportsReasoning != tt.wantReasoning {
				t.Errorf("SupportsReasoning = %v, want %v", got.SupportsReasoning, tt.wantReasoning)
			}
		})
	}
}

func TestResolveAgentPricing_Average(t *testing.T) {
	tests := []struct {
		name           string
		agentName      string
		agents         map[string]config.AgentConfig
		wantHasPricing bool
		wantInputAvg   float64
		wantOutputAvg  float64
	}{
		{
			name:      "two models with pricing averaged",
			agentName: "agent-both",
			agents: map[string]config.AgentConfig{
				"agent-both": {
					Models: []config.AgentModel{
						{Provider: "deepseek", Model: "deepseek-v4-flash"},
						{Provider: "gemini", Model: "gemini-2.5-flash"},
					},
				},
			},
			wantHasPricing: true,
			wantInputAvg:   0.0875,
			wantOutputAvg:  0.2,
		},
		{
			name:      "one model with pricing, one without",
			agentName: "agent-one",
			agents: map[string]config.AgentConfig{
				"agent-one": {
					Models: []config.AgentModel{
						{Provider: "deepseek", Model: "deepseek-v4-flash"},
						{Provider: "nvidia_free", Model: "nemotron-3-super"},
					},
				},
			},
			wantHasPricing: true,
			wantInputAvg:   0.1,
			wantOutputAvg:  0.1,
		},
		{
			name:      "no models with pricing",
			agentName: "agent-none",
			agents: map[string]config.AgentConfig{
				"agent-none": {
					Models: []config.AgentModel{
						{Provider: "openai", Model: "gpt-4o-mini"},
						{Provider: "openai", Model: "gpt-3.5-turbo"},
					},
				},
			},
			wantHasPricing: false,
		},
		{
			name:           "agent not found",
			agents:         map[string]config.AgentConfig{},
			wantHasPricing: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveAgentPricing(tt.agentName, tt.agents, nil)
			if got.HasPricing != tt.wantHasPricing {
				t.Errorf("HasPricing = %v, want %v", got.HasPricing, tt.wantHasPricing)
			}
			if tt.wantHasPricing {
				const epsilon = 0.0001
				if diff := got.InputCostPer1M - tt.wantInputAvg; diff > epsilon || diff < -epsilon {
					t.Errorf("InputCostPer1M = %v, want %v (diff %v)", got.InputCostPer1M, tt.wantInputAvg, diff)
				}
				if diff := got.OutputCostPer1M - tt.wantOutputAvg; diff > epsilon || diff < -epsilon {
					t.Errorf("OutputCostPer1M = %v, want %v (diff %v)", got.OutputCostPer1M, tt.wantOutputAvg, diff)
				}
			}
		})
	}
}
func TestResolveWindowMaxOutput_AgentLevelOverride(t *testing.T) {
	agents := map[string]config.AgentConfig{
		"test-agent": {
			Models: []config.AgentModel{
				{Provider: "deepseek", Model: "deepseek-v4-flash", MaxOutput: 500000},
				{Provider: "gemini", Model: "gemini-2.5-flash"},
			},
		},
	}
	got := ResolveWindowMaxOutput("test-agent", agents, nil)
	if got != 500000 {
		t.Fatalf("expected 500000 (agent override on deepseek raises max above registry 384000), got %d", got)
	}
}

func TestResolveAgentCapabilities_UnresolvableMetadata(t *testing.T) {
	agents := map[string]config.AgentConfig{
		"agent-unknown": {
			Models: []config.AgentModel{
				{Provider: "gemini", Model: "gemini-2.5-flash"},
				{Provider: "unknown", Model: "nonexistent-model-xyz"},
			},
		},
	}
	got := ResolveAgentCapabilities("agent-unknown", agents, nil)
	if got.SupportsVision {
		t.Error("SupportsVision should be false when one model has unresolvable metadata")
	}
	if got.SupportsToolCalls {
		t.Error("SupportsToolCalls should be false when one model has unresolvable metadata")
	}
	if got.SupportsReasoning {
		t.Error("SupportsReasoning should be false when one model has unresolvable metadata")
	}
}
