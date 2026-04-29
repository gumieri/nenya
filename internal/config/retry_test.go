package config

import (
	"testing"
)

func TestGovernanceConfig_EffectiveMaxRetryAttempts(t *testing.T) {
	tests := []struct {
		name     string
		cfg      GovernanceConfig
		expected int
	}{
		{
			name:     "zero defaults to 3",
			cfg:      GovernanceConfig{},
			expected: 3,
		},
		{
			name:     "negative defaults to 3",
			cfg:      GovernanceConfig{MaxRetryAttempts: -1},
			expected: 3,
		},
		{
			name:     "explicit value used",
			cfg:      GovernanceConfig{MaxRetryAttempts: 5},
			expected: 5,
		},
		{
			name:     "one attempt",
			cfg:      GovernanceConfig{MaxRetryAttempts: 1},
			expected: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.EffectiveMaxRetryAttempts(); got != tt.expected {
				t.Errorf("EffectiveMaxRetryAttempts() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestProviderConfig_MaxRetryAttempts(t *testing.T) {
	tests := []struct {
		name string
		cfg  ProviderConfig
		want int
	}{
		{
			name: "default zero",
			cfg:  ProviderConfig{},
			want: 0,
		},
		{
			name: "explicit value",
			cfg:  ProviderConfig{MaxRetryAttempts: 5},
			want: 5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.MaxRetryAttempts; got != tt.want {
				t.Errorf("MaxRetryAttempts = %v, want %v", got, tt.want)
			}
		})
	}
}
