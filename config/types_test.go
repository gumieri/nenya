package config

import (
	"encoding/json"
	"testing"
)

func TestApiKey_Validate(t *testing.T) {
	tests := []struct {
		name    string
		key     ApiKey
		wantErr bool
	}{
		{
			name: "valid key",
			key: ApiKey{
				Name:    "test-key",
				Token:   "valid-token-1234567890",
				Roles:   []string{"admin"},
				Enabled: true,
			},
			wantErr: false,
		},
		{
			name: "empty token",
			key: ApiKey{
				Token: "",
				Roles: []string{"admin"},
			},
			wantErr: true,
		},
		{
			name: "short token",
			key: ApiKey{
				Token: "short",
				Roles: []string{"admin"},
			},
			wantErr: true,
		},
		{
			name: "no roles",
			key: ApiKey{
				Token: "valid-token-1234567890",
				Roles: []string{},
			},
			wantErr: true,
		},
		{
			name: "invalid role",
			key: ApiKey{
				Token: "valid-token-1234567890",
				Roles: []string{"superadmin"},
			},
			wantErr: true,
		},
		{
			name: "valid expires_at",
			key: ApiKey{
				Token:     "valid-token-1234567890",
				Roles:     []string{"user"},
				ExpiresAt: "2026-12-31T23:59:59Z",
				Enabled:   true,
			},
			wantErr: false,
		},
		{
			name: "invalid expires_at format",
			key: ApiKey{
				Token:     "valid-token-1234567890",
				Roles:     []string{"user"},
				ExpiresAt: "not-a-date",
				Enabled:   true,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.key.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestIsValidRole(t *testing.T) {
	tests := []struct {
		role     string
		expected bool
	}{
		{"admin", true},
		{"user", true},
		{"read-only", true},
		{"superadmin", false},
		{"", false},
		{"admin ", false},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			if got := isValidRole(tt.role); got != tt.expected {
				t.Errorf("isValidRole(%q) = %v, want %v", tt.role, got, tt.expected)
			}
		})
	}
}

func TestAutoAgentsConfig_IsEnabled(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *AutoAgentsConfig
		category string
		expected bool
	}{
		{
			name:     "nil config",
			cfg:      nil,
			category: "fast",
			expected: true,
		},
		{
			name: "fast category true",
			cfg: &AutoAgentsConfig{
				Fast: &AutoAgentCategoryConfig{Enabled: true},
			},
			category: "fast",
			expected: true,
		},
		{
			name: "fast category false",
			cfg: &AutoAgentsConfig{
				Fast: &AutoAgentCategoryConfig{Enabled: false},
			},
			category: "fast",
			expected: false,
		},
		{
			name:     "unknown category",
			cfg:      &AutoAgentsConfig{},
			category: "unknown",
			expected: false,
		},
		{
			name: "category with nil config",
			cfg: &AutoAgentsConfig{
				Reasoning: &AutoAgentCategoryConfig{Enabled: true},
			},
			category: "reasoning",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IsEnabled(tt.category); got != tt.expected {
				t.Errorf("IsEnabled(%q) = %v, want %v", tt.category, got, tt.expected)
			}
		})
	}
}

func TestAutoAgentsConfig_IsEnabled_AllCategories(t *testing.T) {
	cfg := &AutoAgentsConfig{
		Fast:      &AutoAgentCategoryConfig{Enabled: true},
		Reasoning: &AutoAgentCategoryConfig{Enabled: false},
		Vision:    &AutoAgentCategoryConfig{Enabled: true},
		Tools:     &AutoAgentCategoryConfig{Enabled: false},
		Large:     &AutoAgentCategoryConfig{Enabled: true},
		Balanced:  &AutoAgentCategoryConfig{Enabled: false},
		Coding:    &AutoAgentCategoryConfig{Enabled: true},
	}

	cases := map[string]bool{
		"fast":      true,
		"reasoning": false,
		"vision":    true,
		"tools":     false,
		"large":     true,
		"balanced":  false,
		"coding":    true,
	}

	for category, expected := range cases {
		t.Run(category, func(t *testing.T) {
			if got := cfg.IsEnabled(category); got != expected {
				t.Errorf("IsEnabled(%q) = %v, want %v", category, got, expected)
			}
		})
	}
}

func TestEffectiveMaxRetryAttempts(t *testing.T) {
	tests := []struct {
		name     string
		g        GovernanceConfig
		expected int
	}{
		{
			name:     "default zero",
			g:        GovernanceConfig{},
			expected: 3,
		},
		{
			name:     "explicit value",
			g:        GovernanceConfig{MaxRetryAttempts: 5},
			expected: 5,
		},
		{
			name:     "negative value",
			g:        GovernanceConfig{MaxRetryAttempts: -1},
			expected: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.g.EffectiveMaxRetryAttempts(); got != tt.expected {
				t.Errorf("EffectiveMaxRetryAttempts() = %d, want %d", got, tt.expected)
			}
		})
	}
}

func TestPtrTo(t *testing.T) {
	intVal := PtrTo(42)
	if *intVal != 42 {
		t.Errorf("expected 42, got %d", *intVal)
	}

	boolVal := PtrTo(true)
	if *boolVal != true {
		t.Errorf("expected true, got %v", *boolVal)
	}

	strVal := PtrTo("hello")
	if *strVal != "hello" {
		t.Errorf("expected hello, got %s", *strVal)
	}
}

func TestBouncerConfig_UnmarshalJSON_AutoEnableWithPatterns(t *testing.T) {
	data := `{"redact_patterns": ["pattern1", "pattern2"]}`
	var cfg BouncerConfig
	if err := json.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Enabled == nil || !*cfg.Enabled {
		t.Error("expected auto-enabled when patterns are set")
	}
	if len(cfg.RedactPatterns) != 2 {
		t.Errorf("expected 2 patterns, got %d", len(cfg.RedactPatterns))
	}
}

func TestEngineRef_UnmarshalJSON_InvalidJSON(t *testing.T) {
	var ref EngineRef
	err := ref.UnmarshalJSON([]byte("{invalid"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestWithValue(t *testing.T) {
	tests := []struct {
		name  string
		input *int
		want  bool
	}{
		{
			name:  "non-nil",
			input: PtrTo(5),
			want:  true,
		},
		{
			name:  "nil",
			input: nil,
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wasSet(tt.input)
			if got != tt.want {
				t.Errorf("wasSet(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
